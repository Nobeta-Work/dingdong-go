// Command trade 是独立部署的服务入口，只负责将启动错误映射为进程退出码。
package main

import (
	"log/slog"
	"os"

	"github.com/Nobeta-Work/dingdong-go/internal/trade"
)

func main() {
	if err := trade.Run(); err != nil {
		slog.Error("service_failed", "service", "trade-service", "error", err)
		os.Exit(1)
	}
}
