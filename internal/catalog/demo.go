package catalog

import (
	"strconv"

	pb "github.com/Nobeta-Work/dingdong-go/api/gen/catalog/v1"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/app"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/auth"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/httpx"
	"github.com/gin-gonic/gin"
)

// registerDemo 为每个验收场景创建独立库存，并复用真实库存事务验证乱序。
// ! 仅显式开启演示开关并通过运营鉴权时可用，不提供任意 SQL 或库存重置入口。
func registerDemo(r *app.Runtime, s *inventoryService) {
	if !r.Config.DemoFaults {
		return
	}
	g := r.Router.Group("/api/admin/demo", auth.Require(r.Config.JWTSecret, true))
	g.POST("/skus", func(c *gin.Context) {
		var input struct {
			Name  string `json:"name" binding:"required,max=128"`
			Stock int    `json:"stock" binding:"min=1,max=10000"`
		}
		if c.ShouldBindJSON(&input) != nil {
			httpx.Write(c, 400, "INVALID_ARGUMENT", "名称或库存不合法", nil)
			return
		}
		tx, err := r.DB.BeginTx(c.Request.Context(), nil)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(c.Request.Context(), "INSERT INTO sku(name,price_cent) VALUES (?,1990)", input.Name)
		var id int64
		if err == nil {
			id, err = result.LastInsertId()
		}
		if err == nil {
			_, err = tx.ExecContext(c.Request.Context(), "INSERT INTO inventory(sku_id,total,available) VALUES (?,?,?)", id, input.Stock, input.Stock)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			httpx.Error(c, err)
			return
		}
		httpx.Write(c, 201, "OK", "演示 SKU 已创建", gin.H{"skuId": strconv.FormatInt(id, 10), "stock": input.Stock})
	})
	g.POST("/inventory/:action", func(c *gin.Context) {
		var input struct {
			OrderNo string `json:"orderNo" binding:"required,max=64"`
			SKUID   string `json:"skuId" binding:"required"`
		}
		if c.ShouldBindJSON(&input) != nil || httpx.ID(input.SKUID) != nil {
			httpx.Write(c, 400, "INVALID_ARGUMENT", "业务键不合法", nil)
			return
		}
		var result *pb.Reservation
		var err error
		ctx := c.Request.Context()
		in := &pb.InventoryRequest{OrderNo: input.OrderNo, SkuId: input.SKUID, Quantity: 1}
		key := &pb.ReservationRequest{OrderNo: input.OrderNo, SkuId: input.SKUID}
		switch c.Param("action") {
		case "reserve":
			result, err = s.Reserve(ctx, in)
		case "release":
			result, err = s.Release(ctx, in)
		case "confirm":
			result, err = s.Confirm(ctx, key)
		case "query":
			result, err = s.GetReservation(ctx, key)
		default:
			httpx.Write(c, 404, "NOT_FOUND", "未知动作", nil)
			return
		}
		if err != nil {
			httpx.Error(c, err)
			return
		}
		httpx.Write(c, 200, "OK", "演示库存事务完成", gin.H{"orderNo": result.OrderNo, "skuId": result.SkuId, "status": result.Status.String()})
	})
}
