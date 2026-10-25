// Package payment 支付服务拥有模拟支付单和支付事件，通过本地事务维护互斥终态。
package payment

import "github.com/Nobeta-Work/dingdong-go/internal/platform/app"

// Run 启动支付服务的 HTTP、RPC 和数据库依赖，模拟支付业务在当前包内实现。
func Run() error {
	return app.Run(app.Spec{Name: "payment-service", MQ: true, RPC: true, Configure: configure})
}
