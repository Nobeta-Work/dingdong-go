package event

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	rocketmq "github.com/apache/rocketmq-client-go/v2"
	"github.com/apache/rocketmq-client-go/v2/consumer"
	"github.com/apache/rocketmq-client-go/v2/primitive"
	"github.com/apache/rocketmq-client-go/v2/producer"
	"github.com/google/uuid"
)

// Put 将事件与调用方业务变更放在同一本地事务。事件 ID 由类型和聚合键确定，重复写入不重置投递状态。
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

// Bus 只管理当前服务所属库的事件投递。业务解释交给 handle，不共享服务间的数据写入规则。
type Bus struct {
	db       *sql.DB
	producer rocketmq.Producer
	consumer rocketmq.PushConsumer
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	logger   *slog.Logger
	owner    string
}

// Start 先建立 MQ 连接，再启动本地 Outbox 扫描；handle 为 nil 时只生产消息。
func Start(db *sql.DB, nameserver, group string, logger *slog.Logger, handle func(context.Context, Envelope) error) (*Bus, error) {
	p, err := rocketmq.NewProducer(producer.WithNameServer([]string{nameserver}), producer.WithGroupName(group+"-producer"), producer.WithRetry(0))
	if err != nil {
		return nil, err
	}
	if err = p.Start(); err != nil {
		return nil, err
	}
	b := &Bus{db: db, producer: p, logger: logger, owner: uuid.NewString()}
	if handle != nil {
		c, err := rocketmq.NewPushConsumer(consumer.WithNameServer([]string{nameserver}), consumer.WithGroupName(group),
			consumer.WithConsumeFromWhere(consumer.ConsumeFromFirstOffset), consumer.WithConsumeMessageBatchMaxSize(1), consumer.WithConsumeGoroutineNums(8))
		if err != nil {
			_ = p.Shutdown()
			return nil, err
		}
		err = c.Subscribe("dingdong-events", consumer.MessageSelector{}, func(parent context.Context, messages ...*primitive.MessageExt) (consumer.ConsumeResult, error) {
			for _, m := range messages {
				var e Envelope
				if err := json.Unmarshal(m.Body, &e); err != nil {
					logger.Error("invalid_event", "error", err)
					return consumer.ConsumeRetryLater, nil
				}
				ctx, cancel := context.WithTimeout(parent, 10*time.Second)
				err := handle(ctx, e)
				cancel()
				if err != nil {
					logger.Warn("event_retry", "eventId", e.EventID, "eventType", e.EventType, "orderNo", e.AggregateID, "traceId", e.TraceID, "error", err)
					return consumer.ConsumeRetryLater, nil
				}
				logger.Info("event_consumed", "eventId", e.EventID, "eventType", e.EventType, "orderNo", e.AggregateID, "traceId", e.TraceID)
			}
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
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			for count := 0; count < 100; count++ {
				if ctx.Err() != nil {
					return
				}
				found, err := b.dispatch(ctx)
				if err != nil {
					logger.Warn("outbox_retry", "error", err)
					break
				}
				if !found {
					break
				}
			}
		}
	}()
	return b, nil
}

func (b *Bus) dispatch(parent context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var id uint64
	var body []byte
	err = tx.QueryRowContext(ctx, `SELECT id,payload FROM outbox WHERE
		(status='PENDING' AND next_attempt_at<=UTC_TIMESTAMP(3)) OR
		(status='PROCESSING' AND lease_until<UTC_TIMESTAMP(3)) ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id, &body)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE outbox SET status='PROCESSING',lease_owner=?,lease_until=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 30 SECOND),attempts=attempts+1 WHERE id=?", b.owner, id)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}

	// * 网络发送在短事务之外；发送成功但确认写库失败时允许重发，由业务幂等吸收。
	result, sendErr := b.producer.SendSync(ctx, primitive.NewMessage("dingdong-events", body))
	if sendErr == nil && result.Status != primitive.SendOK {
		sendErr = fmt.Errorf("Broker 未确认消息: %v", result.Status)
	}
	if sendErr != nil {
		_, err = b.db.ExecContext(ctx, "UPDATE outbox SET status='PENDING',next_attempt_at=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 5 SECOND),last_error=?,lease_owner=NULL,lease_until=NULL WHERE id=? AND lease_owner=?", truncate(sendErr.Error()), id, b.owner)
		if err != nil {
			return true, err
		}
		return true, sendErr
	}
	_, err = b.db.ExecContext(ctx, "UPDATE outbox SET status='SENT',last_error='',lease_owner=NULL,lease_until=NULL WHERE id=? AND lease_owner=?", id, b.owner)
	return true, err
}

func truncate(s string) string {
	r := []rune(s)
	if len(r) > 500 {
		return string(r[:500])
	}
	return s
}

// Close 停止领取新 Outbox，再等待投递与消费退出；截止后由进程退出触发 MQ 重投。
func (b *Bus) Close(ctx context.Context) error {
	b.cancel()
	done := make(chan struct{})
	go func() {
		if b.consumer != nil {
			_ = b.consumer.Shutdown()
		}
		b.wg.Wait()
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
