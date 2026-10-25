package trade

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Nobeta-Work/dingdong-go/internal/platform/event"
)

func (s *service) handleEvent(ctx context.Context, e event.Envelope) error {
	if e.SchemaVersion != 1 {
		return fmt.Errorf("暂不支持事件版本 %d", e.SchemaVersion)
	}
	switch e.EventType {
	case "CreateOrderRequested":
		return s.createOrder(ctx, e.AggregateID, e.TraceID)
	case "PaymentSucceeded":
		return s.paymentSucceeded(ctx, e.AggregateID, e.TraceID)
	case "OrderExpiryDue":
		return s.closeOrder(ctx, e.AggregateID, e.TraceID)
	case "InventoryConfirmRequested":
		return s.settleInventory(ctx, e.AggregateID, true)
	case "InventoryReleaseRequested":
		return s.settleInventory(ctx, e.AggregateID, false)
	default:
		return fmt.Errorf("未知事件类型 %q", e.EventType)
	}
}

// recoverLoop 采用游标分页扫描持久化中间态，进程重启无需恢复任何内存订单。
// M1 的重复扫描通过业务键、行锁和终态幂等收敛；M2 再补充完整任务租约及故障验收。
func (s *service) recoverLoop(ctx context.Context, logger *slog.Logger) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var requestCursor, orderCursor, taskCursor uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.scanRequests(ctx, logger, &requestCursor)
		s.scanOrders(ctx, logger, &orderCursor)
		s.scanTasks(ctx, logger, &taskCursor)
	}
}

func (s *service) scanRequests(parent context.Context, logger *slog.Logger, cursor *uint64) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	rows, err := s.db.QueryContext(ctx, `SELECT id,order_no FROM activity_request WHERE id>? AND status IN ('ACCEPTED','PROCESSING')
		AND updated_at<DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 10 SECOND) ORDER BY id LIMIT 100`, *cursor)
	if err != nil {
		cancel()
		logger.Warn("request_scan_failed", "error", err)
		return
	}
	type entry struct {
		id uint64
		no string
	}
	batch := make([]entry, 0, 100)
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.id, &e.no); err != nil {
			break
		}
		batch = append(batch, e)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	cancel()
	if err != nil {
		logger.Warn("request_scan_failed", "error", err)
		return
	}
	if len(batch) == 0 {
		*cursor = 0
		return
	}
	for _, e := range batch {
		if parent.Err() != nil {
			return
		}
		*cursor = e.id
		ctx, cancel := context.WithTimeout(parent, 10*time.Second)
		err := s.createOrder(ctx, e.no, "recovery")
		cancel()
		if err != nil {
			logger.Warn("request_recovery_retry", "orderNo", e.no, "error", err)
		}
	}
}

func (s *service) scanOrders(parent context.Context, logger *slog.Logger, cursor *uint64) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	rows, err := s.db.QueryContext(ctx, `SELECT id,order_no,status FROM orders WHERE id>? AND
		((status IN ('PENDING_PAYMENT','CLOSING') AND expires_at<=UTC_TIMESTAMP(3)) OR
		(status IN ('PAID','CANCELED') AND inventory_status='LOCKED')) ORDER BY id LIMIT 100`, *cursor)
	if err != nil {
		cancel()
		logger.Warn("order_scan_failed", "error", err)
		return
	}
	type entry struct {
		id        uint64
		no, state string
	}
	batch := make([]entry, 0, 100)
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.id, &e.no, &e.state); err != nil {
			break
		}
		batch = append(batch, e)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	cancel()
	if err != nil {
		logger.Warn("order_scan_failed", "error", err)
		return
	}
	if len(batch) == 0 {
		*cursor = 0
		return
	}
	for _, e := range batch {
		if parent.Err() != nil {
			return
		}
		*cursor = e.id
		ctx, cancel := context.WithTimeout(parent, 10*time.Second)
		if e.state == "PAID" || e.state == "CANCELED" {
			err = s.settleInventory(ctx, e.no, e.state == "PAID")
		} else {
			err = s.closeOrder(ctx, e.no, "recovery")
		}
		cancel()
		if err != nil {
			logger.Warn("order_recovery_retry", "orderNo", e.no, "error", err)
		}
	}
}

func (s *service) scanTasks(parent context.Context, logger *slog.Logger, cursor *uint64) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	rows, err := s.db.QueryContext(ctx, "SELECT id,business_key FROM operation_task WHERE id>? AND status='PENDING' AND action='RELEASE_BEFORE_ORDER' AND next_attempt_at<=UTC_TIMESTAMP(3) ORDER BY id LIMIT 100", *cursor)
	if err != nil {
		cancel()
		logger.Warn("task_scan_failed", "error", err)
		return
	}
	type entry struct {
		id uint64
		no string
	}
	batch := make([]entry, 0, 100)
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.id, &e.no); err != nil {
			break
		}
		batch = append(batch, e)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	cancel()
	if err != nil {
		logger.Warn("task_scan_failed", "error", err)
		return
	}
	if len(batch) == 0 {
		*cursor = 0
		return
	}
	for _, e := range batch {
		if parent.Err() != nil {
			return
		}
		*cursor = e.id
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		err = s.settleInventory(ctx, e.no, false)
		if err != nil {
			_, _ = s.db.ExecContext(ctx, "UPDATE operation_task SET attempts=attempts+1,next_attempt_at=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 5 SECOND) WHERE id=? AND status='PENDING'", e.id)
			logger.Warn("task_recovery_retry", "orderNo", e.no, "error", err)
		}
		cancel()
	}
}
