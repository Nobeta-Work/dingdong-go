package catalog

import (
	"context"
	"database/sql"

	pb "github.com/Nobeta-Work/dingdong-go/api/gen/catalog/v1"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/app"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/auth"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/httpx"
	"github.com/gin-gonic/gin"
)

type catalogService struct {
	pb.UnimplementedCatalogServer
	db *sql.DB
}

func (s *catalogService) GetSKU(ctx context.Context, in *pb.GetSKURequest) (*pb.SKU, error) {
	if err := httpx.ID(in.SkuId); err != nil {
		return nil, err
	}
	sku := &pb.SKU{}
	err := s.db.QueryRowContext(ctx, "SELECT s.id,s.name,s.price_cent,s.enabled,i.available FROM sku s JOIN inventory i ON i.sku_id=s.id WHERE s.id=?", in.SkuId).Scan(&sku.SkuId, &sku.Name, &sku.PriceCent, &sku.Available, &sku.StockAvailable)
	return sku, rpcError(err)
}

func configure(r *app.Runtime) (func(context.Context) error, error) {
	s := &catalogService{db: r.DB}
	pb.RegisterCatalogServer(r.RPC, s)
	pb.RegisterInventoryServer(r.RPC, &inventoryService{db: r.DB})
	r.Router.GET("/api/skus/:id", func(c *gin.Context) {
		sku, err := s.GetSKU(c.Request.Context(), &pb.GetSKURequest{SkuId: c.Param("id")})
		if err != nil {
			httpx.Error(c, err)
			return
		}
		httpx.Write(c, 200, "OK", "查询成功", gin.H{"skuId": sku.SkuId, "name": sku.Name, "priceCent": sku.PriceCent, "available": sku.Available})
	})
	r.Router.GET("/api/admin/skus/:id/inventory", auth.Require(r.Config.JWTSecret, true), func(c *gin.Context) {
		if err := httpx.ID(c.Param("id")); err != nil {
			httpx.Error(c, err)
			return
		}
		var total, available, locked, sold int
		err := r.DB.QueryRowContext(c.Request.Context(), "SELECT total,available,locked,sold FROM inventory WHERE sku_id=?", c.Param("id")).Scan(&total, &available, &locked, &sold)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		httpx.Write(c, 200, "OK", "查询成功", gin.H{"skuId": c.Param("id"), "total": total, "available": available, "locked": locked, "sold": sold})
	})
	return nil, nil
}
