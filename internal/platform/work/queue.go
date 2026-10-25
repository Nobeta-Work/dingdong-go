// Package work 实现所属库内的短事务领取和有界重试，不解释商城业务。
package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Nobeta-Work/dingdong-go/internal/platform/metrics"
	"github.com/google/uuid"
)

// Item 是一次领取的不可变快照。Token 每次领取重新生成，防止过期执行者覆盖新领取结果。
type Item struct {
	ID       uint64
	Payload  []byte
	Attempts int
	Token    string
	Key      string
	Action   string
	TraceID  string
}

// Queue 只接受固定的三种基础设施表，表名不会来自外部请求。
type Queue struct {
	DB          *sql.DB
	Table       string
	MaxAttempts int
	metrics     *metrics.Registry
	logger      *slog.Logger
	handle      func(context.Context, Item) error
	cancel      context.CancelFunc
	wg          sync.WaitGroup
}

// New 创建队列，领取租约 15 秒，单次工作最多 10 秒，网络调用必须放在领取事务之外。
func New(db *sql.DB, table string, maxAttempts int, registry *metrics.Registry, logger *slog.Logger, handle func(context.Context, Item) error) *Queue {
	if table != "outbox" && table != "event_inbox" && table != "operation_task" {
		panic("未知工作表")
	}
	return &Queue{DB: db, Table: table, MaxAttempts: maxAttempts, metrics: registry, logger: logger, handle: handle}
}

// Start 启动固定数量的工作协程；空队列每 200ms 检查一次，避免无限创建 goroutine。
func (q *Queue) Start(workers int) {
	ctx, cancel := context.WithCancel(context.Background())
	q.cancel = cancel
	for i := 0; i < workers; i++ {
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			for ctx.Err() == nil {
				found, err := q.RunOne(ctx)
				if err != nil {
					q.logger.Warn("queue_retry", "queue", q.Table, "error", err)
				}
				if !found || err != nil {
					timer := time.NewTimer(200 * time.Millisecond)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
			}
		}()
	}
}

// RunOne 领取一项、提交领取事务、执行处理，再按本次 Token 条件更新。
func (q *Queue) RunOne(parent context.Context) (bool, error) {
	claimCtx, cancel := context.WithTimeout(parent, 2*time.Second)
	item, found, err := q.claim(claimCtx)
	cancel()
	if err != nil || !found {
		return found, err
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	started := time.Now()
	err = q.handle(ctx, item)
	cancel()
	q.metrics.Observe(q.Table+".processing", time.Since(started))
	// 处理上下文可能已超时；保存重试结果使用独立的有界上下文。
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer finishCancel()
	state := "DONE"
	if q.Table == "outbox" {
		state = "SENT"
	}
	next := time.Now().UTC()
	message := ""
	if err != nil {
		q.logger.Warn("work_failed", "queue", q.Table, "id", item.ID, "businessKey", item.Key, "action", item.Action, "traceId", item.TraceID, "attempt", item.Attempts, "error", err)
		state = "PENDING"
		if item.Attempts >= q.MaxAttempts {
			state = "NEEDS_REVIEW"
		}
		delay := 200 * time.Millisecond
		for attempt := 1; attempt < item.Attempts && delay < 5*time.Second; attempt++ {
			delay *= 2
		}
		if delay > 5*time.Second {
			delay = 5 * time.Second
		}
		next = next.Add(delay)
		runes := []rune(err.Error())
		if len(runes) > 500 {
			runes = runes[:500]
		}
		message = string(runes)
		q.metrics.Add(q.Table+".retry", 1)
	}
	result, saveErr := q.DB.ExecContext(finishCtx, "UPDATE "+q.Table+" SET status=?,next_attempt_at=?,last_error=?,lease_owner=NULL,lease_until=NULL WHERE id=? AND status='PROCESSING' AND lease_owner=?", state, next, message, item.ID, item.Token)
	if saveErr != nil {
		return true, saveErr
	}
	n, saveErr := result.RowsAffected()
	if saveErr != nil {
		return true, saveErr
	}
	if n != 1 {
		return true, fmt.Errorf("%s 工作租约已失效，旧执行结果未覆盖新领取者", q.Table)
	}
	q.metrics.Add(q.Table+"."+strings.ToLower(state), 1)
	return true, err
}

func (q *Queue) claim(ctx context.Context) (Item, bool, error) {
	tx, err := q.DB.BeginTx(ctx, nil)
	if err != nil {
		return Item{}, false, err
	}
	defer tx.Rollback()
	var item Item
	extra := ""
	if q.Table == "operation_task" {
		extra = ",business_key,action,trace_id"
	}
	row := tx.QueryRowContext(ctx, "SELECT id,payload,attempts"+extra+" FROM "+q.Table+` WHERE
		(status='PENDING' AND next_attempt_at<=UTC_TIMESTAMP(3)) OR (status='PROCESSING' AND lease_until<=UTC_TIMESTAMP(3))
		ORDER BY next_attempt_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`)
	args := []any{&item.ID, &item.Payload, &item.Attempts}
	if q.Table == "operation_task" {
		args = append(args, &item.Key, &item.Action, &item.TraceID)
	}
	err = row.Scan(args...)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, err
	}
	if item.Attempts >= q.MaxAttempts {
		_, err = tx.ExecContext(ctx, "UPDATE "+q.Table+" SET status='NEEDS_REVIEW',last_error='工作多次退出或租约过期，需要人工恢复',lease_owner=NULL,lease_until=NULL WHERE id=?", item.ID)
		if err != nil {
			return Item{}, false, err
		}
		return Item{}, false, tx.Commit()
	}
	item.Token = uuid.NewString()
	item.Attempts++
	_, err = tx.ExecContext(ctx, "UPDATE "+q.Table+" SET status='PROCESSING',attempts=attempts+1,lease_owner=?,lease_until=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 15 SECOND) WHERE id=?", item.Token, item.ID)
	if err != nil {
		return Item{}, false, err
	}
	return item, true, tx.Commit()
}

// Stop 立即停止新领取。等待由 Close 完成，便于同时停止消费者和其他任务。
func (q *Queue) Stop() {
	if q.cancel != nil {
		q.cancel()
	}
}

// Close 等待协程退出，不在超过退出预算后无限等待。
func (q *Queue) Close(ctx context.Context) error {
	q.Stop()
	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PutTask 与业务状态共用本地事务。重复事件可提前唤醒 PENDING，不重置 DONE 或 NEEDS_REVIEW。
func PutTask(ctx context.Context, tx *sql.Tx, key, action string, payload any, due time.Time, trace string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO operation_task (business_key,action,payload,next_attempt_at,trace_id) VALUES (?,?,?,?,?)
		ON DUPLICATE KEY UPDATE next_attempt_at=IF(status='PENDING',LEAST(next_attempt_at,VALUES(next_attempt_at)),next_attempt_at)`, key, action, string(body), due.UTC(), trace)
	return err
}
