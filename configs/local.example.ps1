# 先用 compose.yaml + compose.debug.yaml 启动依赖与 catalog/payment，停止 trade 容器后调试。
# Broker 公告 host.docker.internal，本机也必须能解析该名称；具体说明见 README。
# . .\configs\local.example.ps1
# go run ./cmd/trade
$env:MYSQL_DSN = 'dingdong_trade:demo-trade@tcp(localhost:13306)/dingdong_trade'
$env:JWT_SECRET = 'dingdong-demo-signing-key-change-me-32bytes'
$env:PAYMENT_WINDOW = '120s'
$env:HTTP_ADDR = ':18082'
$env:REDIS_ADDR = 'localhost:16379'
$env:CATALOG_RPC_TARGET = 'dns:///localhost:19000'
$env:PAYMENT_RPC_TARGET = 'dns:///localhost:19001'
$env:ROCKETMQ_NAMESERVER = 'localhost:19876'
# 本机探测通过映射端口进行；真正的 MQ 客户端使用 NameServer 返回的 Broker 公告地址。
$env:ROCKETMQ_BROKER_ADDR = 'localhost:10911'
