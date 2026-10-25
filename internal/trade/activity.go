package trade

import (
	"context"
	"strconv"
	"time"

	catalogpb "github.com/Nobeta-Work/dingdong-go/api/gen/catalog/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type createActivity struct {
	Title     string    `json:"title" binding:"required,max=128"`
	SKUID     string    `json:"skuId" binding:"required"`
	PriceCent int64     `json:"priceCent" binding:"gt=0"`
	Quota     int       `json:"quota" binding:"gt=0,lte=1000000"`
	StartsAt  time.Time `json:"startsAt" binding:"required"`
	EndsAt    time.Time `json:"endsAt" binding:"required"`
}

func (s *service) createActivity(ctx context.Context, in createActivity) (activity, error) {
	if !in.EndsAt.After(in.StartsAt) {
		return activity{}, status.Error(codes.InvalidArgument, "结束时间必须晚于开始时间")
	}
	rpcCtx, cancel := rpcContext(ctx)
	sku, err := s.catalog.GetSKU(rpcCtx, &catalogpb.GetSKURequest{SkuId: in.SKUID})
	cancel()
	if err != nil {
		return activity{}, err
	}
	if !sku.Available {
		return activity{}, status.Error(codes.FailedPrecondition, "商品未启用")
	}
	res, err := s.db.ExecContext(ctx, "INSERT INTO activity (title,sku_id,sku_name,price_cent,quota,starts_at,ends_at) VALUES (?,?,?,?,?,?,?)", in.Title, sku.SkuId, sku.Name, in.PriceCent, in.Quota, in.StartsAt.UTC(), in.EndsAt.UTC())
	if err != nil {
		return activity{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return activity{}, err
	}
	return getActivity(ctx, s.db, strconv.FormatInt(id, 10), false)
}

func (s *service) publish(ctx context.Context, id string) (activity, error) {
	_, err := s.db.ExecContext(ctx, "UPDATE activity SET status='PUBLISHING' WHERE id=? AND status='DRAFT'", id)
	if err != nil {
		return activity{}, err
	}
	a, err := getActivity(ctx, s.db, id, false)
	if err != nil {
		return activity{}, err
	}
	if a.Status != "PUBLISHED" && a.Status != "PUBLISHING" {
		return activity{}, status.Error(codes.FailedPrecondition, "当前状态不能发布")
	}
	if a.Status == "PUBLISHED" && a.AdmissionState == "ACTIVE" && s.metadataReady(ctx, a) {
		return a, nil
	}
	if a.Status == "PUBLISHING" {
		rpcCtx, cancel := rpcContext(ctx)
		sku, err := s.catalog.GetSKU(rpcCtx, &catalogpb.GetSKURequest{SkuId: a.SKUID})
		cancel()
		if err != nil {
			return activity{}, err
		}
		if !sku.Available || a.Quota > int(sku.StockAvailable) {
			return activity{}, status.Error(codes.FailedPrecondition, "发布额度超过当前可用库存或商品未启用")
		}
	}
	a, err = s.scheduleRebuild(ctx, id)
	if err != nil {
		return activity{}, err
	}
	if err = s.buildAdmission(ctx, id, a.AdmissionEpoch); err != nil {
		return activity{}, err
	}
	return getActivity(ctx, s.db, id, false)
}

func (s *service) pause(ctx context.Context, id string) (activity, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return activity{}, err
	}
	defer tx.Rollback()
	a, err := getActivity(ctx, tx, id, true)
	if err != nil {
		return activity{}, err
	}
	if a.Status != "PUBLISHED" && a.Status != "PAUSED" {
		return activity{}, status.Error(codes.FailedPrecondition, "只有已发布活动能暂停")
	}
	_, err = tx.ExecContext(ctx, "UPDATE activity SET status='PAUSED',admission_revision=admission_revision+1 WHERE id=?", id)
	if err != nil {
		return activity{}, err
	}
	if err = s.invalidateTask(ctx, tx, id); err != nil {
		return activity{}, err
	}
	if err = tx.Commit(); err != nil {
		return activity{}, err
	}
	// 交易校验以数据库为准；展示失效失败已有持久化任务继续重试。
	_ = s.invalidate(ctx, id)
	return getActivity(ctx, s.db, id, false)
}

func (s *service) updateTitle(ctx context.Context, id, title string) (activity, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return activity{}, err
	}
	defer tx.Rollback()
	if _, err = getActivity(ctx, tx, id, true); err != nil {
		return activity{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE activity SET title=? WHERE id=?", title, id); err != nil {
		return activity{}, err
	}
	if err = s.invalidateTask(ctx, tx, id); err != nil {
		return activity{}, err
	}
	if err = tx.Commit(); err != nil {
		return activity{}, err
	}
	_ = s.invalidate(ctx, id)
	return getActivity(ctx, s.db, id, false)
}
