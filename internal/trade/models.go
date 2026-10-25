package trade

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	catalogpb "github.com/Nobeta-Work/dingdong-go/api/gen/catalog/v1"
	paymentpb "github.com/Nobeta-Work/dingdong-go/api/gen/payment/v1"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/app"
	"github.com/redis/go-redis/v9"
)

// service 持有本服务数据库及长期复用的 RPC 客户端，不直接访问商品库或支付库。
type service struct {
	db        *sql.DB
	catalog   catalogpb.CatalogClient
	inventory catalogpb.InventoryClient
	payment   paymentpb.PaymentClient
	window    time.Duration
	redis     *redis.Client
	logger    *slog.Logger
}

func newService(r *app.Runtime) *service {
	return &service{db: r.DB, catalog: catalogpb.NewCatalogClient(r.CatalogConn), inventory: catalogpb.NewInventoryClient(r.CatalogConn),
		payment: paymentpb.NewPaymentClient(r.PaymentConn), window: r.Config.PaymentWindow, redis: r.Redis, logger: r.Logger}
}

type activity struct {
	ID                string    `json:"id"`
	Title             string    `json:"title"`
	SKUID             string    `json:"skuId"`
	SKUName           string    `json:"skuName"`
	PriceCent         int64     `json:"priceCent"`
	Quota             int       `json:"quota"`
	UsedQuota         int       `json:"usedQuota"`
	StartsAt          time.Time `json:"startsAt"`
	EndsAt            time.Time `json:"endsAt"`
	Version           int64     `json:"version"`
	Status            string    `json:"status"`
	Phase             string    `json:"phase"`
	ParticipationRule string    `json:"participationRule"`
}

const activityColumns = "id,title,sku_id,sku_name,price_cent,quota,used_quota,starts_at,ends_at,version,status"

type request struct {
	ID                  string   `json:"requestId"`
	UserID              string   `json:"userId"`
	ActivityID          string   `json:"activityId"`
	OrderNo             string   `json:"orderNo"`
	Status              string   `json:"status"`
	FailureReason       string   `json:"failureReason"`
	QualificationStatus string   `json:"qualificationStatus,omitempty"`
	Snapshot            activity `json:"-"`
	ParameterHash       string   `json:"-"`
}

type order struct {
	OrderNo         string    `json:"orderNo"`
	ActivityID      string    `json:"activityId"`
	UserID          string    `json:"userId"`
	AmountCent      int64     `json:"amountCent"`
	ExpiresAt       time.Time `json:"expiresAt"`
	CreatedAt       time.Time `json:"createdAt"`
	Status          string    `json:"status"`
	InventoryStatus string    `json:"inventoryStatus"`
	Fulfillable     bool      `json:"fulfillable"`
	SKUID           string    `json:"skuId"`
	SKUName         string    `json:"skuName"`
	PriceCent       int64     `json:"priceCent"`
	Quantity        int       `json:"quantity"`
}

type rowScanner interface{ Scan(...any) error }
type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func scanActivity(row rowScanner) (activity, error) {
	var a activity
	err := row.Scan(&a.ID, &a.Title, &a.SKUID, &a.SKUName, &a.PriceCent, &a.Quota, &a.UsedQuota, &a.StartsAt, &a.EndsAt, &a.Version, &a.Status)
	a.Phase = "ACTIVE"
	if time.Now().Before(a.StartsAt) {
		a.Phase = "UPCOMING"
	} else if !time.Now().Before(a.EndsAt) {
		a.Phase = "ENDED"
	}
	a.ParticipationRule = "每位用户每个活动最多一张订单；订单超时只释放 SKU 库存，不恢复活动资格或额度"
	return a, err
}

func getActivity(ctx context.Context, q rowQuerier, id string, lock bool) (activity, error) {
	sqlText := "SELECT " + activityColumns + " FROM activity WHERE id=?"
	if lock {
		sqlText += " FOR UPDATE"
	}
	return scanActivity(q.QueryRowContext(ctx, sqlText, id))
}

// rpcContext 给每一次同步 RPC 单独设置 1 秒截止，不启用全局写操作透明重试。
func rpcContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, time.Second)
}
