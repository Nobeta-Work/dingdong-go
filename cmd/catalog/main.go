// Command catalog 是独立部署的服务入口，只负责将启动错误映射为进程退出码。
package main

import (
	"log/slog"
	"os"

	"github.com/Nobeta-Work/dingdong-go/internal/catalog"
)

func main() {
	if err := catalog.Run(); err != nil {
		slog.Error("service_failed", "service", "catalog-service", "error", err)
		os.Exit(1)
	}
}
