// Command mqcheck 验证真实 RocketMQ 生产、消费和 Broker 重投，不运行商城业务。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	rocketmq "github.com/apache/rocketmq-client-go/v2"
	"github.com/apache/rocketmq-client-go/v2/consumer"
	"github.com/apache/rocketmq-client-go/v2/primitive"
	"github.com/apache/rocketmq-client-go/v2/producer"
	"github.com/google/uuid"
)

func main() {
	if err := run(); err != nil {
		slog.Error("mq_check_failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	addr := os.Getenv("ROCKETMQ_NAMESERVER")
	if addr == "" {
		addr = "localhost:19876"
	}
	id := uuid.NewString()
	const topic = "dingdong-framework-check"
	// 每次独立消费组，从最早位置读取并按本次 ID 过滤，避免首次消费起点竞争。
	// 这些临时组仅用于 Demo 检查，长期环境应由运维清理。
	c, err := rocketmq.NewPushConsumer(consumer.WithNameServer([]string{addr}),
		consumer.WithGroupName("framework-check-"+id), consumer.WithConsumeFromWhere(consumer.ConsumeFromFirstOffset),
		consumer.WithConsumeMessageBatchMaxSize(1), consumer.WithConsumeGoroutineNums(1), consumer.WithMaxReconsumeTimes(3))
	if err != nil {
		return err
	}
	done := make(chan struct{}, 1)
	err = c.Subscribe(topic, consumer.MessageSelector{}, func(ctx context.Context, messages ...*primitive.MessageExt) (consumer.ConsumeResult, error) {
		for _, msg := range messages {
			if string(msg.Body) != id {
				continue
			}
			slog.Info("mq_received", "eventId", id, "reconsumeTimes", msg.ReconsumeTimes)
			if msg.ReconsumeTimes == 0 {
				// * 主动拒绝第一次投递；必须收到 Broker 的重投才判定检查通过。
				cc, ok := primitive.GetConcurrentlyCtx(ctx)
				if ok {
					cc.DelayLevelWhenNextConsume = 1
				}
				return consumer.ConsumeRetryLater, nil
			}
			select {
			case done <- struct{}{}:
			default:
			}
		}
		return consumer.ConsumeSuccess, nil
	})
	if err != nil {
		return err
	}
	if err := c.Start(); err != nil {
		return err
	}
	defer c.Shutdown()
	p, err := rocketmq.NewProducer(producer.WithNameServer([]string{addr}), producer.WithGroupName("framework-check-producer-"+id), producer.WithRetry(0))
	if err != nil {
		return err
	}
	if err := p.Start(); err != nil {
		return err
	}
	defer p.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sendCtx, sendCancel := context.WithTimeout(ctx, 5*time.Second)
	defer sendCancel()
	result, err := p.SendSync(sendCtx, primitive.NewMessage(topic, []byte(id)))
	if err != nil {
		return fmt.Errorf("发送检查消息: %w", err)
	}
	if result.Status != primitive.SendOK {
		return fmt.Errorf("Broker 未确认发送: %v", result.Status)
	}
	slog.Info("mq_sent", "eventId", id, "messageId", result.MsgID)
	select {
	case <-done:
		slog.Info("mq_check_passed", "eventId", id, "verified", "produce_consume_redelivery")
		return nil
	case <-ctx.Done():
		return fmt.Errorf("60 秒内未收到重投: %w", ctx.Err())
	}
}
