// Package catalog 商品服务拥有 SKU、库存和预占流水，通过本地事务保证库存守恒。
package catalog

import "github.com/Nobeta-Work/dingdong-go/internal/platform/app"

// Run 启动商品服务的 HTTP、RPC 和数据库依赖，库存业务在当前包内实现。
func Run() error {
	return app.Run(app.Spec{Name: "catalog-service", RPC: true, Configure: configure})
}
