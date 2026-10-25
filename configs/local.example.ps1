# 先启动 Compose 的依赖、迁移以及 catalog/payment；再在单独终端运行本文件调试 trade。
# . .\configs\local.example.ps1
# go run ./cmd/trade
$env:MYSQL_DSN = 'dingdong_trade:demo-trade@tcp(localhost:13306)/dingdong_trade'
$env:HTTP_ADDR = ':18082'
$env:REDIS_ADDR = 'localhost:16379'
$env:CATALOG_RPC_TARGET = 'dns:///localhost:19000'
$env:PAYMENT_RPC_TARGET = 'dns:///localhost:19001'
$env:ROCKETMQ_NAMESERVER = 'localhost:19876'
# 框架只做 TCP 检查；通过 Compose 的临时工具容器完成真实 MQ 通信。
# Broker 已映射到本机回环地址，仅用于就绪探测；mqcheck 仍须在容器内运行。
$env:ROCKETMQ_BROKER_ADDR = 'localhost:10911'
