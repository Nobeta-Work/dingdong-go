package event

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Nobeta-Work/dingdong-go/internal/platform/fault"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/metrics"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/work"
	rocketmq "github.com/apache/rocketmq-client-go/v2"
	"github.com/apache/rocketmq-client-go/v2/consumer"
	"github.com/apache/rocketmq-client-go/v2/primitive"
	"github.com/apache/rocketmq-client-go/v2/producer"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Put 与业务变更共用本地事务；相同类型与聚合键生成同一事件 ID，重投不重置投递状态。
func Put(ctx context.Context, tx *sql.Tx, kind, aggregate string, payload any, due time.Time, trace string) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	e := Envelope{EventID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(kind+":"+aggregate)).String(), EventType: kind,
		AggregateID: aggregate, OccurredAt: time.Now().UTC(), SchemaVersion: 1, Payload: data, TraceID: trace}
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO outbox (event_id,aggregate_id,event_type,payload,trace_id,schema_version,occurred_at,next_attempt_at)
		VALUES (?,?,?,?,?,1,?,?) ON DUPLICATE KEY UPDATE event_id=event_id`, e.EventID, aggregate, kind, string(body), trace, e.OccurredAt, due.UTC())
	return err
}

// Options 将固定并发、重试阈值和 Demo 故障点注入消息基础设施。
type Options struct {
	Workers     int
	MaxAttempts int
	Metrics     *metrics.Registry
	Faults      *fault.Engine
}

// Bus 使用 Outbox 保证发送可恢复、Inbox 保证确认 MQ 前消息已持久化。
// Inbox 的技术失败有界退避，达到阈值保留 NEEDS_REVIEW，不能丢消息或伪装成功。
type Bus struct {
	producer rocketmq.Producer
	consumer rocketmq.PushConsumer
	outbox   *work.Queue
	inbox    *work.Queue
	options  Options
}

// Start 启动真实 MQ SDK 和所属库的持久化队列。业务处理不在 SDK 消费回调中长时间运行。
func Start(db *sql.DB, nameserver, group string, logger *slog.Logger, handle func(context.Context, Envelope) error, options Options) (*Bus, error) {
	p, err := rocketmq.NewProducer(producer.WithNameServer([]string{nameserver}), producer.WithGroupName(group+"-producer"), producer.WithRetry(0))
	if err != nil {
		return nil, err
	}
	if err = p.Start(); err != nil {
		return nil, err
	}
	b := &Bus{producer: p, options: options}
	prefix := strings.TrimPrefix(group, "dingdong-")
	b.outbox = work.New(db, "outbox", options.MaxAttempts, options.Metrics, logger, func(ctx context.Context, item work.Item) error {
		var e Envelope
		if err := json.Unmarshal(item.Payload, &e); err != nil {
			return err
		}
		if err := options.Faults.Hit(ctx, prefix+".before_outbox_send", e.AggregateID); err != nil {
			return err
		}
		result, err := p.SendSync(ctx, primitive.NewMessage("dingdong-events", item.Payload))
		if err != nil {
			return err
		}
		if result.Status != primitive.SendOK {
			return fmt.Errorf("Broker 未确认发送: %v", result.Status)
		}
		// 发送已成功但本地尚未标记 SENT，故障后允许同一 eventId 再投。
		if err = options.Faults.Hit(ctx, prefix+".after_outbox_send", e.AggregateID); err != nil {
			return err
		}
		options.Metrics.Add("mq.sent", 1)
		logger.Info("event_sent", "eventId", e.EventID, "eventType", e.EventType, "orderNo", e.AggregateID, "traceId", e.TraceID)
		return nil
	})
	if handle != nil {
		b.inbox = work.New(db, "event_inbox", options.MaxAttempts, options.Metrics, logger, func(ctx context.Context, item work.Item) error {
			var e Envelope
			if err := json.Unmarshal(item.Payload, &e); err != nil {
				return err
			}
			if err := handle(ctx, e); err != nil {
				return err
			}
			options.Metrics.Add("mq.processed", 1)
			logger.Info("event_consumed", "eventId", e.EventID, "eventType", e.EventType, "orderNo", e.AggregateID, "traceId", e.TraceID)
			return nil
		})
		c, err := rocketmq.NewPushConsumer(consumer.WithNameServer([]string{nameserver}), consumer.WithGroupName(group),
			consumer.WithConsumeFromWhere(consumer.ConsumeFromFirstOffset), consumer.WithConsumeMessageBatchMaxSize(1),
			consumer.WithConsumeGoroutineNums(options.Workers), consumer.WithMaxReconsumeTimes(2147483647))
		if err != nil {
			_ = p.Shutdown()
			return nil, err
		}
		err = c.Subscribe("dingdong-events", consumer.MessageSelector{}, func(parent context.Context, messages ...*primitive.MessageExt) (consumer.ConsumeResult, error) {
			for _, message := range messages {
				var e Envelope
				err := json.Unmarshal(message.Body, &e)
				id := e.EventID
				_, idErr := uuid.Parse(id)
				if err != nil || idErr != nil {
					id = uuid.NewSHA1(uuid.NameSpaceOID, message.Body).String()
				}
				ctx, cancel := context.WithTimeout(parent, 3*time.Second)
				result, err := db.ExecContext(ctx, "INSERT INTO event_inbox (event_id,payload) VALUES (?,?) ON DUPLICATE KEY UPDATE deliveries=deliveries+1", id, string(message.Body))
				cancel()
				if err != nil {
					logger.Warn("inbox_persist_retry", "eventId", id, "error", err)
					return consumer.ConsumeRetryLater, nil
				}
				if n, _ := result.RowsAffected(); n != 1 {
					options.Metrics.Add("mq.duplicate", 1)
				}
			}
			// * ACK 的依据是消息已可靠入库，业务终态由 Inbox/任务队列继续推进。
			return consumer.ConsumeSuccess, nil
		})
		if err == nil {
			err = c.Start()
		}
		if err != nil {
			_ = p.Shutdown()
			return nil, err
		}
		b.consumer = c
		b.inbox.Start(options.Workers)
	}
	b.outbox.Start(1)
	return b, nil
}

// Register 挂载当前服务实际拥有的消息队列管理接口。
func (b *Bus) Register(router *gin.Engine, secret string) {
	b.outbox.Register(router, secret, b.options.Faults.Enabled)
	if b.inbox != nil {
		b.inbox.Register(router, secret, false)
	}
}

// Close 先停止领取，再等待在途工作，最后关闭长期复用的 MQ 连接。
func (b *Bus) Close(ctx context.Context) error {
	b.outbox.Stop()
	if b.inbox != nil {
		b.inbox.Stop()
	}
	done := make(chan struct{})
	go func() {
		if b.consumer != nil {
			_ = b.consumer.Shutdown()
		}
		_ = b.outbox.Close(ctx)
		if b.inbox != nil {
			_ = b.inbox.Close(ctx)
		}
		_ = b.producer.Shutdown()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
