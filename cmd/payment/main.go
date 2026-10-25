// Command payment 是独立部署的服务入口，只负责将启动错误映射为进程退出码。
package main

import (
	"log/slog"
	"os"

	"github.com/Nobeta-Work/dingdong-go/internal/payment"
)

func main() {
	if err := payment.Run(); err != nil {
		slog.Error("service_failed", "service", "payment-service", "error", err)
		os.Exit(1)
	}
}
