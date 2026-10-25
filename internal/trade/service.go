// Package trade 交易服务拥有活动、受理和订单，依赖 Redis、MQ 与库存/支付 RPC。
package trade

import "github.com/Nobeta-Work/dingdong-go/internal/platform/app"

// Run 启动交易服务及其上下游连接，活动与订单业务在当前包内实现。
func Run() error {
	return app.Run(app.Spec{Name: "trade-service", Redis: true, MQ: true, TradeClients: true})
}
