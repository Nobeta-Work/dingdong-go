package trade

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/Nobeta-Work/dingdong-go/internal/platform/event"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/httpx"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/work"
	"github.com/google/uuid"
)

// handleEvent 先把 MQ 消息转成所属库的任务。重复消息不重置已完成或待人工检查的任务。
func (s *service) handleEvent(ctx context.Context, e event.Envelope) error {
	if e.SchemaVersion != 1 || e.EventID == "" || e.AggregateID == "" {
		return fmt.Errorf("事件字段或版本不合法")
	}
	if _, err := uuid.Parse(e.EventID); err != nil || len(e.AggregateID) > 64 {
		return fmt.Errorf("事件业务键不合法")
	}
	switch e.EventType {
	case "CreateOrderRequested", "PaymentSucceeded", "OrderExpiryDue", "InventoryConfirmRequested", "InventoryReleaseRequested":
	default:
		return fmt.Errorf("未知事件类型 %q", e.EventType)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = work.PutTask(ctx, tx, e.AggregateID, e.EventType, map[string]string{"orderNo": e.AggregateID}, time.Now(), e.TraceID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *service) handleTask(ctx context.Context, item work.Item) error {
	ctx = httpx.WithTrace(ctx, item.TraceID)
	var payload map[string]string
	if err := json.Unmarshal(item.Payload, &payload); err != nil {
		return err
	}
	switch item.Action {
	case "CreateOrderRequested":
		return s.createOrder(ctx, item.Key, item.TraceID)
	case "PaymentSucceeded":
		return s.paymentSucceeded(ctx, item.Key, item.TraceID)
	case "OrderExpiryDue":
		return s.closeOrder(ctx, item.Key, item.TraceID)
	case "InventoryConfirmRequested":
		return s.settleInventory(ctx, item.Key, true)
	case "InventoryReleaseRequested":
		return s.settleInventory(ctx, item.Key, false)
	case "InvalidateActivityCache":
		return s.invalidate(ctx, payload["activityId"])
	case "RebuildAdmission":
		return s.buildAdmission(ctx, payload["activityId"], payload["epoch"])
	case "AcknowledgeAdmission", "RefundAdmission":
		r, err := readRequest(ctx, s.db, payload["userId"], payload["requestId"])
		if err != nil {
			return err
		}
		action := "ACK"
		if item.Action == "RefundAdmission" {
			if r.Status != "FAILED" && r.ActivityID == payload["activityId"] {
				return fmt.Errorf("请求未落失败终态，不允许回补")
			}
			action = "REFUND"
		}
		return s.resolveAdmission(ctx, payload["activityId"], payload["userId"], payload["requestId"], action)
	default:
		return fmt.Errorf("未知任务动作 %q", item.Action)
	}
}

// recoverLoop 只分页补齐遗留/到期任务，网络动作由有租约与并发上限的工作队列执行。
func (s *service) recoverLoop(ctx context.Context, logger *slog.Logger) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var requestCursor, orderCursor, activityCursor uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := s.scanBusiness(ctx, &requestCursor, &orderCursor); err != nil {
			logger.Warn("business_scan_retry", "error", err)
		}
		if err := s.scanAdmission(ctx, &activityCursor); err != nil {
			logger.Warn("admission_scan_retry", "error", err)
		}
	}
}

func (s *service) scanBusiness(parent context.Context, requestCursor, orderCursor *uint64) error {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	type entry struct {
		id         uint64
		no, action string
	}
	batch := make([]entry, 0, 200)
	rows, err := s.db.QueryContext(ctx, "SELECT id,order_no FROM activity_request WHERE id>? AND status IN ('ACCEPTED','PROCESSING') AND updated_at<DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 10 SECOND) ORDER BY id LIMIT 100", *requestCursor)
	if err != nil {
		return err
	}
	n := 0
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.id, &e.no); err != nil {
			rows.Close()
			return err
		}
		e.action = "CreateOrderRequested"
		batch = append(batch, e)
		*requestCursor = e.id
		n++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if n == 0 {
		*requestCursor = 0
	}
	rows, err = s.db.QueryContext(ctx, `SELECT id,order_no,status FROM orders WHERE id>? AND
		((status IN ('PENDING_PAYMENT','CLOSING') AND expires_at<=UTC_TIMESTAMP(3)) OR (status IN ('PAID','CANCELED') AND inventory_status='LOCKED')) ORDER BY id LIMIT 100`, *orderCursor)
	if err != nil {
		return err
	}
	n = 0
	for rows.Next() {
		var e entry
		var state string
		if err = rows.Scan(&e.id, &e.no, &state); err != nil {
			rows.Close()
			return err
		}
		e.action = "OrderExpiryDue"
		if state == "PAID" {
			e.action = "InventoryConfirmRequested"
		}
		if state == "CANCELED" {
			e.action = "InventoryReleaseRequested"
		}
		batch = append(batch, e)
		*orderCursor = e.id
		n++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if n == 0 {
		*orderCursor = 0
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range batch {
		if err = work.PutTask(ctx, tx, e.no, e.action, map[string]string{"orderNo": e.no}, time.Now(), "recovery"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *service) scanAdmission(parent context.Context, cursor *uint64) error {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, "SELECT "+activityColumns+" FROM activity WHERE id>? AND status IN ('PUBLISHING','PUBLISHED','PAUSED') ORDER BY id LIMIT 100", *cursor)
	if err != nil {
		return err
	}
	batch := make([]activity, 0, 100)
	for rows.Next() {
		a, scanErr := scanActivity(rows)
		if scanErr != nil {
			rows.Close()
			return scanErr
		}
		batch = append(batch, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(batch) == 0 {
		*cursor = 0
		return nil
	}
	for _, a := range batch {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var id uint64
		if _, err = fmt.Sscan(a.ID, &id); err != nil {
			return err
		}
		*cursor = id
		if a.AdmissionState != "ACTIVE" || !s.metadataReady(ctx, a) {
			if _, err = s.scheduleRebuild(ctx, a.ID); err != nil {
				return err
			}
			continue
		}
		pending, err := pendingScript.Run(ctx, s.redis, admissionKeys(a.ID)).StringSlice()
		if err != nil {
			_, rebuildErr := s.scheduleRebuild(ctx, a.ID)
			if rebuildErr != nil {
				return rebuildErr
			}
			continue
		}
		for _, body := range pending {
			var t ticket
			if err = json.Unmarshal([]byte(body), &t); err != nil {
				return err
			}
			if err = s.terminalizeHold(ctx, t); err != nil && err != sql.ErrNoRows {
				return err
			}
		}
	}
	return nil
}
