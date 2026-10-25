package trade

import (
	"context"
	"database/sql"
	"errors"
	"time"

	catalogpb "github.com/Nobeta-Work/dingdong-go/api/gen/catalog/v1"
	paymentpb "github.com/Nobeta-Work/dingdong-go/api/gen/payment/v1"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/event"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/work"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const orderColumns = "o.order_no,o.activity_id,o.user_id,o.amount_cent,o.expires_at,o.created_at,o.status,o.inventory_status,i.sku_id,i.sku_name,i.price_cent,i.quantity"
const orderJoin = " FROM orders o JOIN order_items i ON i.order_no=o.order_no "

func scanOrder(row rowScanner) (order, error) {
	var o order
	err := row.Scan(&o.OrderNo, &o.ActivityID, &o.UserID, &o.AmountCent, &o.ExpiresAt, &o.CreatedAt, &o.Status, &o.InventoryStatus, &o.SKUID, &o.SKUName, &o.PriceCent, &o.Quantity)
	o.Fulfillable = o.Status == "PAID" && o.InventoryStatus == "CONFIRMED"
	return o, err
}

func readOrder(ctx context.Context, q rowQuerier, no string) (order, error) {
	return scanOrder(q.QueryRowContext(ctx, "SELECT "+orderColumns+orderJoin+" WHERE o.order_no=?", no))
}

func (s *service) createOrder(ctx context.Context, no, trace string) error {
	var userID, requestID string
	err := s.db.QueryRowContext(ctx, "SELECT user_id,request_id FROM activity_request WHERE order_no=?", no).Scan(&userID, &requestID)
	if err != nil {
		return err
	}
	r, err := readRequest(ctx, s.db, userID, requestID)
	if err != nil {
		return err
	}
	if r.Status == "ORDER_CREATED" || r.Status == "FAILED" {
		return nil
	}
	_, err = s.db.ExecContext(ctx, "UPDATE activity_request SET status='PROCESSING' WHERE order_no=? AND status='ACCEPTED'", no)
	if err != nil {
		return err
	}
	rpcCtx, cancel := rpcContext(ctx)
	_, err = s.inventory.Reserve(rpcCtx, &catalogpb.InventoryRequest{OrderNo: no, SkuId: r.Snapshot.SKUID, Quantity: 1})
	cancel()
	if err != nil && status.Code(err) != codes.ResourceExhausted && status.Code(err) != codes.FailedPrecondition {
		// 发送超时不能判断是否预占；先用同一业务键回查，再决定继续还是保留处理中。
		reserveErr := err
		rpcCtx, cancel = rpcContext(ctx)
		reservation, queryErr := s.inventory.GetReservation(rpcCtx, &catalogpb.ReservationRequest{OrderNo: no, SkuId: r.Snapshot.SKUID})
		cancel()
		if queryErr != nil {
			return reserveErr
		}
		switch reservation.Status {
		case catalogpb.ReservationStatus_RESERVATION_STATUS_LOCKED, catalogpb.ReservationStatus_RESERVATION_STATUS_CONFIRMED:
			err = nil
		case catalogpb.ReservationStatus_RESERVATION_STATUS_RELEASED:
			err = status.Error(codes.FailedPrecondition, "预占已被终止")
		default:
			return reserveErr
		}
	}
	if err != nil {
		if status.Code(err) == codes.ResourceExhausted || status.Code(err) == codes.FailedPrecondition {
			return s.failRequest(ctx, r, status.Convert(err).Message(), trace)
		}
		// ! 超时或网络失败可能已预占成功，保留 PROCESSING，按同一 orderNo 幂等重试。
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	err = tx.QueryRowContext(ctx, "SELECT status FROM activity_request WHERE order_no=? FOR UPDATE", no).Scan(&state)
	if err != nil {
		return err
	}
	if state == "ORDER_CREATED" || state == "FAILED" {
		return nil
	}
	// 成单不重新检查活动时间；合法受理跨过结束时间仍按持久化快照处理。
	now := time.Now().UTC().Truncate(time.Millisecond)
	expires := now.Add(s.window).Truncate(time.Millisecond)
	_, err = tx.ExecContext(ctx, `INSERT INTO orders (order_no,activity_id,user_id,amount_cent,expires_at,created_at,status)
		VALUES (?,?,?,?,?,?,'PENDING_PAYMENT')`, no, r.ActivityID, userID, r.Snapshot.PriceCent, expires, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO order_items (order_no,sku_id,sku_name,price_cent,quantity) VALUES (?,?,?,?,1)", no, r.Snapshot.SKUID, r.Snapshot.SKUName, r.Snapshot.PriceCent)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "UPDATE activity_participation SET status='CONSUMED' WHERE activity_id=? AND user_id=? AND holder_request_id=? AND status='HELD'", r.ActivityID, userID, requestID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return status.Error(codes.FailedPrecondition, "受理资格持有者不一致")
	}
	_, err = tx.ExecContext(ctx, "UPDATE activity_request SET status='ORDER_CREATED' WHERE order_no=?", no)
	if err != nil {
		return err
	}
	// 到期事件与兜底任务同时持久化；MQ 正常投递时会提前唤醒本地任务。
	if err = event.Put(ctx, tx, "OrderExpiryDue", no, map[string]string{"orderNo": no}, expires, trace); err != nil {
		return err
	}
	if err = work.PutTask(ctx, tx, no, "OrderExpiryDue", map[string]string{"orderNo": no}, expires.Add(5*time.Second), trace); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.metrics.Add("orders.created", 1)
	return s.faults.Hit(ctx, "trade.after_order_commit", no)
}

func (s *service) failRequest(ctx context.Context, r request, reason, trace string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 与新受理保持相同的 activity -> request/qualification 加锁方向。
	if _, err = getActivity(ctx, tx, r.ActivityID, true); err != nil {
		return err
	}
	var state string
	err = tx.QueryRowContext(ctx, "SELECT status FROM activity_request WHERE order_no=? FOR UPDATE", r.OrderNo).Scan(&state)
	if err != nil {
		return err
	}
	if state == "ORDER_CREATED" || state == "FAILED" {
		return nil
	}
	_, err = tx.ExecContext(ctx, "UPDATE activity_request SET status='FAILED',failure_reason=? WHERE order_no=?", reason, r.OrderNo)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "UPDATE activity_participation SET status='AVAILABLE',holder_request_id=NULL WHERE activity_id=? AND user_id=? AND holder_request_id=? AND status='HELD'", r.ActivityID, r.UserID, r.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		_, err = tx.ExecContext(ctx, "UPDATE activity SET used_quota=used_quota-1,admission_revision=admission_revision+1 WHERE id=?", r.ActivityID)
		if err != nil {
			return err
		}
	}
	// 落失败后再可靠释放；即使本次没有预占，也要落终止标记阻挡迟到的同键 Reserve。
	if err = event.Put(ctx, tx, "InventoryReleaseRequested", r.OrderNo, map[string]string{"orderNo": r.OrderNo}, time.Now(), trace); err != nil {
		return err
	}
	if err = work.PutTask(ctx, tx, r.OrderNo, "InventoryReleaseRequested", map[string]string{"orderNo": r.OrderNo}, time.Now().Add(5*time.Second), trace); err != nil {
		return err
	}
	if err = work.PutTask(ctx, tx, r.OrderNo, "RefundAdmission", map[string]string{"activityId": r.ActivityID, "userId": r.UserID, "requestId": r.ID}, time.Now(), trace); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.metrics.Add("requests.failed", 1)
	return nil
}

func (s *service) createPayment(ctx context.Context, no, userID string) (*paymentpb.PaymentRecord, error) {
	o, err := readOrder(ctx, s.db, no)
	if err != nil {
		return nil, err
	}
	if o.UserID != userID {
		return nil, status.Error(codes.PermissionDenied, "不能支付他人订单")
	}
	if o.Status != "PENDING_PAYMENT" || !time.Now().Before(o.ExpiresAt) {
		return nil, status.Error(codes.FailedPrecondition, "订单当前不可支付")
	}
	rpcCtx, cancel := rpcContext(ctx)
	p, err := s.payment.CreateOrGet(rpcCtx, &paymentpb.CreatePaymentRequest{OrderNo: no, UserId: userID, AmountCent: o.AmountCent, ExpiresAtUnixMilli: o.ExpiresAt.UnixMilli()})
	cancel()
	if err != nil {
		if status.Code(err) == codes.InvalidArgument || status.Code(err) == codes.FailedPrecondition {
			return nil, err
		}
		createErr := err
		rpcCtx, cancel = rpcContext(ctx)
		p, err = s.payment.QueryByOrder(rpcCtx, &paymentpb.OrderRequest{OrderNo: no})
		cancel()
		if err != nil {
			return nil, createErr
		}
		if p.Status != paymentpb.PaymentStatus_PAYMENT_STATUS_CLOSED && (p.UserId != userID || p.AmountCent != o.AmountCent || p.ExpiresAtUnixMilli != o.ExpiresAt.UnixMilli()) {
			return nil, status.Error(codes.FailedPrecondition, "支付事实与订单参数不一致")
		}
	}
	if p.Status == paymentpb.PaymentStatus_PAYMENT_STATUS_CLOSED {
		return nil, status.Error(codes.FailedPrecondition, "支付单已关闭")
	}
	if p.Status == paymentpb.PaymentStatus_PAYMENT_STATUS_PENDING && time.Now().UnixMilli() >= p.ExpiresAtUnixMilli {
		return nil, status.Error(codes.FailedPrecondition, "支付窗口已结束")
	}
	return p, nil
}

func (s *service) paymentSucceeded(ctx context.Context, no, trace string) error {
	rpcCtx, cancel := rpcContext(ctx)
	p, err := s.payment.QueryByOrder(rpcCtx, &paymentpb.OrderRequest{OrderNo: no})
	cancel()
	if err != nil {
		return err
	}
	o, err := readOrder(ctx, s.db, no)
	if err != nil {
		return err
	}
	if p.Status != paymentpb.PaymentStatus_PAYMENT_STATUS_SUCCESS || p.UserId != o.UserID || p.AmountCent != o.AmountCent {
		return status.Error(codes.FailedPrecondition, "支付事实与订单不一致")
	}
	return s.finishOrder(ctx, no, true, trace)
}

func (s *service) closeOrder(ctx context.Context, no, trace string) error {
	o, err := readOrder(ctx, s.db, no)
	if err != nil {
		return err
	}
	if o.Status == "PAID" || o.Status == "CANCELED" {
		return nil
	}
	if time.Now().Before(o.ExpiresAt) {
		return nil
	}
	_, err = s.db.ExecContext(ctx, "UPDATE orders SET status='CLOSING' WHERE order_no=? AND status='PENDING_PAYMENT' AND expires_at<=UTC_TIMESTAMP(3)", no)
	if err != nil {
		return err
	}
	o, err = readOrder(ctx, s.db, no)
	if err != nil {
		return err
	}
	if o.Status != "CLOSING" {
		return nil
	}
	rpcCtx, cancel := rpcContext(ctx)
	p, err := s.payment.QueryByOrder(rpcCtx, &paymentpb.OrderRequest{OrderNo: no})
	cancel()
	if err != nil && status.Code(err) != codes.NotFound {
		return err
	}
	if status.Code(err) == codes.NotFound || p.Status == paymentpb.PaymentStatus_PAYMENT_STATUS_PENDING {
		rpcCtx, cancel = rpcContext(ctx)
		p, err = s.payment.CloseByOrder(rpcCtx, &paymentpb.OrderRequest{OrderNo: no})
		cancel()
		if err != nil {
			return err
		}
	}
	if p.Status == paymentpb.PaymentStatus_PAYMENT_STATUS_SUCCESS {
		return s.paymentSucceeded(ctx, no, trace)
	}
	if p.Status != paymentpb.PaymentStatus_PAYMENT_STATUS_CLOSED {
		return status.Error(codes.Unavailable, "支付关闭结果未知")
	}
	return s.finishOrder(ctx, no, false, trace)
}

func (s *service) finishOrder(ctx context.Context, no string, paid bool, trace string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current string
	err = tx.QueryRowContext(ctx, "SELECT status FROM orders WHERE order_no=? FOR UPDATE", no).Scan(&current)
	if err != nil {
		return err
	}
	terminal, kind := "CANCELED", "InventoryReleaseRequested"
	if paid {
		terminal, kind = "PAID", "InventoryConfirmRequested"
	}
	if (current == "PAID" || current == "CANCELED") && current != terminal {
		return status.Error(codes.FailedPrecondition, "订单终态与支付事实冲突")
	}
	_, err = tx.ExecContext(ctx, "UPDATE orders SET status=? WHERE order_no=?", terminal, no)
	if err != nil {
		return err
	}
	if err = event.Put(ctx, tx, kind, no, map[string]string{"orderNo": no}, time.Now(), trace); err != nil {
		return err
	}
	if err = work.PutTask(ctx, tx, no, kind, map[string]string{"orderNo": no}, time.Now().Add(5*time.Second), trace); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *service) settleInventory(ctx context.Context, no string, confirm bool) error {
	o, err := readOrder(ctx, s.db, no)
	if errors.Is(err, sql.ErrNoRows) && !confirm {
		// 成单前失败也要释放/终止旧预占；其业务快照保存在受理记录中。
		var userID, requestID string
		err = s.db.QueryRowContext(ctx, "SELECT user_id,request_id FROM activity_request WHERE order_no=? AND status='FAILED'", no).Scan(&userID, &requestID)
		if err != nil {
			return err
		}
		r, err := readRequest(ctx, s.db, userID, requestID)
		if err != nil {
			return err
		}
		rpcCtx, cancel := rpcContext(ctx)
		defer cancel()
		_, err = s.inventory.Release(rpcCtx, &catalogpb.InventoryRequest{OrderNo: no, SkuId: r.Snapshot.SKUID, Quantity: 1})
		if err != nil {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	expected, target := "CANCELED", "RELEASED"
	if confirm {
		expected, target = "PAID", "CONFIRMED"
	}
	if o.Status != expected || o.InventoryStatus == target {
		return nil
	}
	rpcCtx, cancel := rpcContext(ctx)
	if confirm {
		_, err = s.inventory.Confirm(rpcCtx, &catalogpb.ReservationRequest{OrderNo: no, SkuId: o.SKUID})
	} else {
		_, err = s.inventory.Release(rpcCtx, &catalogpb.InventoryRequest{OrderNo: no, SkuId: o.SKUID, Quantity: 1})
	}
	cancel()
	if err != nil {
		writeErr := err
		rpcCtx, cancel = rpcContext(ctx)
		reservation, queryErr := s.inventory.GetReservation(rpcCtx, &catalogpb.ReservationRequest{OrderNo: no, SkuId: o.SKUID})
		cancel()
		expectedReservation := catalogpb.ReservationStatus_RESERVATION_STATUS_RELEASED
		if confirm {
			expectedReservation = catalogpb.ReservationStatus_RESERVATION_STATUS_CONFIRMED
		}
		if queryErr != nil || reservation.Status != expectedReservation {
			return writeErr
		}
	}
	_, err = s.db.ExecContext(ctx, "UPDATE orders SET inventory_status=? WHERE order_no=? AND status=? AND inventory_status='LOCKED'", target, no, expected)
	return err
}
