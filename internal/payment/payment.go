package payment

import (
	"context"
	"database/sql"
	"errors"
	"time"

	pb "github.com/Nobeta-Work/dingdong-go/api/gen/payment/v1"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/event"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/httpx"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type service struct {
	pb.UnimplementedPaymentServer
	db *sql.DB
}

func (s *service) CreateOrGet(ctx context.Context, in *pb.CreatePaymentRequest) (*pb.PaymentRecord, error) {
	if in.OrderNo == "" || len(in.OrderNo) > 64 || httpx.ID(in.UserId) != nil || in.AmountCent <= 0 || in.ExpiresAtUnixMilli <= 0 {
		return nil, status.Error(codes.InvalidArgument, "支付参数不合法")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO payment (order_no,payment_no,user_id,amount_cent,expires_at,status)
		VALUES (?,?,?,?,?,'PENDING') ON DUPLICATE KEY UPDATE order_no=order_no`, in.OrderNo, "P"+uuid.NewString(), in.UserId, in.AmountCent, time.UnixMilli(in.ExpiresAtUnixMilli).UTC())
	if err != nil {
		return nil, err
	}
	p, err := read(ctx, tx, in.OrderNo, true)
	if err != nil {
		return nil, err
	}
	// * 先到的 CLOSED 墓碑拥有最高优先级，不能被迟到的支付创建覆盖。
	if p.Status != pb.PaymentStatus_PAYMENT_STATUS_CLOSED {
		if p.UserId != in.UserId || p.AmountCent != in.AmountCent || p.ExpiresAtUnixMilli != in.ExpiresAtUnixMilli {
			return nil, status.Error(codes.FailedPrecondition, "同一订单的支付参数不一致")
		}
		if p.Status == pb.PaymentStatus_PAYMENT_STATUS_PENDING && time.Now().UnixMilli() >= p.ExpiresAtUnixMilli {
			return nil, status.Error(codes.FailedPrecondition, "支付窗口已结束")
		}
	}
	return p, tx.Commit()
}

func (s *service) QueryByOrder(ctx context.Context, in *pb.OrderRequest) (*pb.PaymentRecord, error) {
	p, err := read(ctx, s.db, in.OrderNo, false)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "支付单尚不存在")
	}
	return p, err
}

func (s *service) CloseByOrder(ctx context.Context, in *pb.OrderRequest) (*pb.PaymentRecord, error) {
	if in.OrderNo == "" || len(in.OrderNo) > 64 {
		return nil, status.Error(codes.InvalidArgument, "缺少订单号")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "INSERT INTO payment (order_no,status) VALUES (?,'CLOSED') ON DUPLICATE KEY UPDATE order_no=order_no", in.OrderNo)
	if err != nil {
		return nil, err
	}
	p, err := read(ctx, tx, in.OrderNo, true)
	if err != nil {
		return nil, err
	}
	if p.Status == pb.PaymentStatus_PAYMENT_STATUS_PENDING {
		if time.Now().UnixMilli() < p.ExpiresAtUnixMilli {
			return nil, status.Error(codes.FailedPrecondition, "支付单尚未到期")
		}
		_, err = tx.ExecContext(ctx, "UPDATE payment SET status='CLOSED' WHERE order_no=? AND status='PENDING'", in.OrderNo)
		if err != nil {
			return nil, err
		}
		p.Status = pb.PaymentStatus_PAYMENT_STATUS_CLOSED
	}
	// 已成功返回 SUCCESS，由交易侧转 PAID；未知结果不允许猜测为 CLOSED。
	return p, tx.Commit()
}

func (s *service) simulate(ctx context.Context, paymentNo, userID, trace string) (*pb.PaymentRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var order string
	err = tx.QueryRowContext(ctx, "SELECT order_no FROM payment WHERE payment_no=?", paymentNo).Scan(&order)
	if err != nil {
		return nil, err
	}
	p, err := read(ctx, tx, order, true)
	if err != nil {
		return nil, err
	}
	if p.UserId != userID {
		return nil, status.Error(codes.PermissionDenied, "不能支付他人订单")
	}
	if p.Status == pb.PaymentStatus_PAYMENT_STATUS_CLOSED {
		return nil, status.Error(codes.FailedPrecondition, "支付单已关闭")
	}
	if p.Status == pb.PaymentStatus_PAYMENT_STATUS_PENDING {
		if time.Now().UnixMilli() >= p.ExpiresAtUnixMilli {
			return nil, status.Error(codes.FailedPrecondition, "支付窗口已结束")
		}
		_, err = tx.ExecContext(ctx, "UPDATE payment SET status='SUCCESS' WHERE order_no=? AND status='PENDING'", order)
		if err != nil {
			return nil, err
		}
		p.Status = pb.PaymentStatus_PAYMENT_STATUS_SUCCESS
	}
	// 重复确认仍幂等写相同事件键；不会跳过第一次尚未投递的成功通知。
	if err = event.Put(ctx, tx, "PaymentSucceeded", order, map[string]string{"orderNo": order}, time.Now(), trace); err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func read(ctx context.Context, q rowQuerier, order string, lock bool) (*pb.PaymentRecord, error) {
	query := "SELECT COALESCE(payment_no,''),order_no,COALESCE(CAST(user_id AS CHAR),''),COALESCE(amount_cent,0),expires_at,status FROM payment WHERE order_no=?"
	if lock {
		query += " FOR UPDATE"
	}
	p := &pb.PaymentRecord{}
	var expires sql.NullTime
	var state string
	err := q.QueryRowContext(ctx, query, order).Scan(&p.PaymentNo, &p.OrderNo, &p.UserId, &p.AmountCent, &expires, &state)
	if expires.Valid {
		p.ExpiresAtUnixMilli = expires.Time.UnixMilli()
	}
	switch state {
	case "PENDING":
		p.Status = pb.PaymentStatus_PAYMENT_STATUS_PENDING
	case "SUCCESS":
		p.Status = pb.PaymentStatus_PAYMENT_STATUS_SUCCESS
	case "CLOSED":
		p.Status = pb.PaymentStatus_PAYMENT_STATUS_CLOSED
	}
	return p, err
}
