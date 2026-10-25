// Package catalog 商品服务拥有 SKU 与库存；对外 RPC 后续在本包实现，禁止跨库访问。
package catalog

import "github.com/Nobeta-Work/dingdong-go/internal/platform/app"

// Run 启动商品服务的 HTTP、RPC 和数据库依赖，库存业务在当前包内实现。
func Run() error {
	return app.Run(app.Spec{Name: "catalog-service", RPC: true})
}
