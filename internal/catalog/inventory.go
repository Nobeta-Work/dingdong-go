package catalog

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/Nobeta-Work/dingdong-go/internal/platform/fault"

	pb "github.com/Nobeta-Work/dingdong-go/api/gen/catalog/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// inventoryService 在一个本地事务内同时修改库存计数与预占流水。
// ! 所有业务拒绝返回明确状态码；网络错误由交易服务按 orderNo 回查，不能当成缺货。
type inventoryService struct {
	pb.UnimplementedInventoryServer
	db     *sql.DB
	faults *fault.Engine
}

func (s *inventoryService) Reserve(ctx context.Context, in *pb.InventoryRequest) (*pb.Reservation, error) {
	return s.change(ctx, in, "RESERVE")
}

func (s *inventoryService) Release(ctx context.Context, in *pb.InventoryRequest) (*pb.Reservation, error) {
	return s.change(ctx, in, "RELEASE")
}

func (s *inventoryService) Confirm(ctx context.Context, in *pb.ReservationRequest) (*pb.Reservation, error) {
	if err := s.faults.Hit(ctx, "catalog.before_confirm", in.OrderNo); err != nil {
		return nil, err
	}
	if in.OrderNo == "" || in.SkuId == "" {
		return nil, status.Error(codes.InvalidArgument, "缺少业务键")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, state, err := readReservation(ctx, tx, in.OrderNo, in.SkuId, true)
	if err != nil {
		return nil, rpcError(err)
	}
	if state == "RELEASED" {
		return nil, status.Error(codes.FailedPrecondition, "库存已释放")
	}
	if state == "LOCKED" {
		_, err = tx.ExecContext(ctx, "UPDATE inventory SET locked=locked-?, sold=sold+? WHERE sku_id=?", r.Quantity, r.Quantity, in.SkuId)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, "UPDATE inventory_reservation SET status='CONFIRMED' WHERE order_no=? AND sku_id=?", in.OrderNo, in.SkuId)
		if err != nil {
			return nil, err
		}
	}
	r.Status = pb.ReservationStatus_RESERVATION_STATUS_CONFIRMED
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return r, s.faults.Hit(ctx, "catalog.after_confirm_commit", in.OrderNo)
}

func (s *inventoryService) GetReservation(ctx context.Context, in *pb.ReservationRequest) (*pb.Reservation, error) {
	r, _, err := readReservation(ctx, s.db, in.OrderNo, in.SkuId, false)
	return r, rpcError(err)
}

func (s *inventoryService) change(ctx context.Context, in *pb.InventoryRequest, action string) (*pb.Reservation, error) {
	if err := s.faults.Hit(ctx, "catalog.before_"+strings.ToLower(action), in.OrderNo); err != nil {
		return nil, err
	}
	if in.OrderNo == "" || len(in.OrderNo) > 64 || in.SkuId == "" || in.Quantity != 1 {
		return nil, status.Error(codes.InvalidArgument, "业务键不能为空且数量固定为 1")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	initial := "LOCKED"
	if action == "RELEASE" {
		initial = "RELEASED"
	}
	// * 唯一键串行化同一订单的预占/释放。释放先到时先写 RELEASED 墓碑，阻止迟到预占。
	res, err := tx.ExecContext(ctx, "INSERT INTO inventory_reservation (order_no,sku_id,quantity,status) VALUES (?,?,?,?) ON DUPLICATE KEY UPDATE id=id", in.OrderNo, in.SkuId, in.Quantity, initial)
	if err != nil {
		return nil, err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	r, state, err := readReservation(ctx, tx, in.OrderNo, in.SkuId, true)
	if err != nil {
		return nil, rpcError(err)
	}
	if r.Quantity != in.Quantity {
		return nil, status.Error(codes.FailedPrecondition, "同一业务键的数量不一致")
	}
	if action == "RESERVE" {
		if state == "RELEASED" {
			return nil, status.Error(codes.FailedPrecondition, "预占已被终止")
		}
		if inserted == 1 {
			result, err := tx.ExecContext(ctx, "UPDATE inventory i JOIN sku s ON s.id=i.sku_id SET i.available=i.available-1,i.locked=i.locked+1 WHERE i.sku_id=? AND i.available>=1 AND s.enabled=TRUE", in.SkuId)
			if err != nil {
				return nil, err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return nil, err
			}
			if n != 1 {
				return nil, status.Error(codes.ResourceExhausted, "SKU_STOCK_UNAVAILABLE")
			}
		}
	} else {
		if state == "CONFIRMED" {
			return nil, status.Error(codes.FailedPrecondition, "已售库存不能释放")
		}
		if inserted == 0 && state == "LOCKED" {
			_, err = tx.ExecContext(ctx, "UPDATE inventory SET locked=locked-1,available=available+1 WHERE sku_id=?", in.SkuId)
			if err != nil {
				return nil, err
			}
			_, err = tx.ExecContext(ctx, "UPDATE inventory_reservation SET status='RELEASED' WHERE order_no=? AND sku_id=?", in.OrderNo, in.SkuId)
			if err != nil {
				return nil, err
			}
		}
		r.Status = pb.ReservationStatus_RESERVATION_STATUS_RELEASED
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	// * 故障发生在提交之后，用于演示“服务已写入、调用方未收到结果”的回查窗口。
	return r, s.faults.Hit(ctx, "catalog.after_"+strings.ToLower(action)+"_commit", in.OrderNo)
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readReservation(ctx context.Context, q rowQuerier, order, sku string, lock bool) (*pb.Reservation, string, error) {
	query := "SELECT order_no,sku_id,quantity,status FROM inventory_reservation WHERE order_no=? AND sku_id=?"
	if lock {
		query += " FOR UPDATE"
	}
	r := &pb.Reservation{}
	var state string
	err := q.QueryRowContext(ctx, query, order, sku).Scan(&r.OrderNo, &r.SkuId, &r.Quantity, &state)
	switch state {
	case "LOCKED":
		r.Status = pb.ReservationStatus_RESERVATION_STATUS_LOCKED
	case "CONFIRMED":
		r.Status = pb.ReservationStatus_RESERVATION_STATUS_CONFIRMED
	case "RELEASED":
		r.Status = pb.ReservationStatus_RESERVATION_STATUS_RELEASED
	}
	return r, state, err
}

func rpcError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return status.Error(codes.NotFound, "对象不存在")
	}
	if err != nil {
		if _, ok := status.FromError(err); !ok {
			return status.Error(codes.Unavailable, "库存数据库暂不可用")
		}
	}
	return err
}
