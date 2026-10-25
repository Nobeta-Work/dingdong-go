# DingDong Go

限量活动商城后端，一个 Go module、四个独立服务。用户浏览活动、提交抢购、查询订单并模拟支付；未付款订单到期后关闭并释放真实 SKU 库存。

**当前状态：M1、M2 业务代码与验收工具已实现，运行验收未执行。** 按本次要求，不启动 Docker 引擎、不运行服务和数据库联调。编译或配置解析不能替代运行验收。接口见 [API.md](API.md)，M2 设计、操作及验证范围见 [M2.md](docs/explain/M2.md)。M3 尚未实现。

学习项目可从 [项目阅读指南](docs/explain/README.md) 开始，按框架、M1 业务、gRPC 和 M2 原理逐步阅读。

## 目录与阅读顺序

```text
cmd/                       四服务入口、seed、mqcheck、diagnose、healthcheck、verify
internal/user/             密码登录、JWT 签发、当前用户
internal/catalog/          SKU 查询、库存预占/确认/释放与流水
internal/trade/            活动、持久化受理、订单、事件消费与恢复扫描
internal/payment/          模拟支付、关闭标记、支付成功事件
internal/platform/         资源生命周期、配置、鉴权、HTTP 协议、事件基础设施
api/proto/ 与 api/gen/      RPC 协议及已生成代码
migrations/{service}/      各服务独立的增量迁移
deploy/                    MySQL 初次建库授权、RocketMQ 配置
configs/                   IDE 配置示例
scripts/                   协议生成、框架检查、业务演示和 M2 验收
compose.yaml               默认容器环境
compose.debug.yaml         Docker Desktop IDE 调试覆盖配置
```

推荐先读 `cmd/trade` → `trade/service.go` → `platform/app` 理解资源生命周期，再按 `trade/http.go` → `request.go` → `order.go` → `worker.go` 理解交易。库存、支付的本地事务分别在 `catalog/inventory.go`、`payment/payment.go`。

HTTP、RPC、MQ 复用各服务内部业务方法；服务只访问自己的库。公共包不承载跨服务业务表写入，也不为尚不存在的复杂度创建空的 repository/interface 层。诊断工具通过独立连接只读比较事实，不执行跨库 SQL。

## 当前业务链路

1. 用户服务校验 bcrypt 密码并签发两小时 JWT。各 HTTP 服务验证 HS256、issuer、有效期和角色，归属只来自可信身份上下文。
2. 运营通过商品 RPC 读取 SKU，创建草稿并发布。发布状态为 DRAFT → PUBLISHING → PUBLISHED，可重试且不重置额度，额度不能超过发布时可用库存。
3. **M2 先重放、限流，再 Redis Lua 预扣，最后数据库事务确认**：锁活动行校验代次、状态、版本、时间和额度，同时持有用户资格、保存受理快照、固定订单号及成单 Outbox，提交后才返回 202。Redis 故障拒绝新受理，已持久化请求仍可重放。预扣未落库先写失败终态再回补，Redis 数据丢失先推进数据库代次再重建。
4. Outbox 投递真实 RocketMQ。消费者按固定订单号预占库存，再本地创建订单、商品快照和到期提醒，消耗用户资格。排队跨过活动结束时间不影响合法受理。
5. 交易服务校验归属与期限后通过 RPC 创建支付单。模拟支付事实和成功事件同事务提交；交易消费后转 PAID，再幂等确认库存。只有 PAID + CONFIRMED 才返回 fulfillable=true。
6. 到期提醒与每五秒扫描共同驱动关闭。CLOSING 中回查支付，已成功则转已支付；支付明确关闭后才取消并释放库存。未知结果保留中间态，RPC 写入超时按业务键回查。取消订单不恢复活动资格或额度。

库存释放先到时写 RELEASED 标记；支付关闭先到时写 CLOSED 标记，防止迟到创建覆盖终态。金额使用分，所有服务按 UTC 存储/读取时间，业务 ID 以字符串输出。

Outbox、Inbox 和操作任务以短事务领取，事务外执行网络调用，每次领取使用独立令牌防止旧执行者覆盖新结果。自动重试有限退避，超过阈值保留 NEEDS_REVIEW，可查看并人工恢复。消费者先可靠写入 Inbox 再确认 MQ，业务继续由持久化队列推进。到期事件在数据库中排期，到期才投递，未接入 Broker 原生延时消息。每五秒扫描分页补齐受理、到期订单和库存任务。

活动详情使用 60～75 秒随机 TTL、5 秒空值缓存、singleflight 二次检查及不同 key 的回源并发预算。展示更新提交后失效缓存，失败有持久化任务。默认用户令牌桶 3/1 token·s⁻¹，活动桶 500/100 token·s⁻¹；429、额度冲突和依赖 503 分开返回。上述机制与多副本、故障和并发验收工具均已实现，运行结果仍待实测。

## 固定版本

| 组件 | 版本 |
| --- | --- |
| Go / 构建镜像 | 1.27.1 / golang:1.27.1-bookworm |
| Gin / JWT | v1.10.0 / golang-jwt/jwt/v5 v5.2.1 |
| grpc-go / Protobuf Go | v1.70.0 / v1.36.5 |
| protoc / Go gRPC 生成器 | 29.3 / v1.5.1 |
| MySQL 驱动 / 服务端 | v1.9.0 / mysql:8.4.4 |
| go-redis / Redis | v9.7.3 / redis:7.4.2-alpine |
| singleflight | golang.org/x/sync v0.10.0 |
| RocketMQ / Go SDK | apache/rocketmq:4.9.8 / rocketmq-client-go/v2 v2.1.2 |
| 迁移工具 | migrate/migrate:v4.18.2 |

MQ 使用 4.x NameServer + Broker，无 Proxy。显式创建 dingdong-events 和 dingdong-framework-check Topic。检查工具主动拒绝首次消费，再检查 Broker 重投，参考 [固定 SDK 官方示例](https://github.com/apache/rocketmq-client-go/blob/v2.1.2/examples/consumer/retry/concurrent/main.go)。镜像与版本组合尚未运行验证。

## 启动与初始化（供后续手动执行）

需要已运行的 Docker Linux 引擎和 Compose v2，脚本不启动 Docker Desktop。建议预留 4 CPU、6 GB 内存，仅为预算建议，未实测资源用量。

仓库根目录首次复制配置，之后不要覆盖已有 .env：

```bash
cp -n .env.example .env
docker compose config --quiet
docker compose up -d --build
docker compose ps -a
docker compose run --rm seed-user
docker compose run --rm seed-catalog
```

`.env` 必须有 MYSQL_ROOT_PASSWORD、至少 32 字节的 JWT_SECRET 和种子账号 DEMO_PASSWORD。PAYMENT_WINDOW 默认 120s，允许 5s～1h，可设 30s 缩短演示；配置变化不修改已有订单 expiresAt。四服务 JWT 密钥必须相同，示例凭据只用于本地 Demo。旧 .env 需手动补入本次新增字段。

默认 Go 代理不可达时，将 GOPROXY 改为 `https://goproxy.cn,direct`，不会修改全局设置；Docker 下载使用其自身网络配置。

MySQL 空卷首次创建四库及独立 DML/DDL 账号。迁移容器执行 up，服务等待所属库迁移成功；M2 新增 `trade/000004_reliability.up.sql`，增加准入代次、任务追踪号和 Inbox。升级前先完成迁移，不能用新二进制配旧表。不要修改已执行迁移。旧卷不会重新执行建库脚本，修改 .env 不会自动更改旧库密码。

种子为 user01、user02、operator、bench0001～bench0500，共 503 个账号，密码取初始化时的 DEMO_PASSWORD 并哈希存储。两个 SKU 各 200 件、1990 分。重复初始化不覆盖账号、商品或库存；活动由演示脚本通过运营接口创建，不绕过发布 RPC 校验。

正常停止使用 `docker compose down`，保留业务数据。**`docker compose down -v` 会删除全部项目数据卷，不属于正常启动或恢复步骤。** 启动、种子和演示脚本均不清空数据、不重置库存或额度。

## 演示与诊断（本次不执行）

Bash 脚本调用真实服务，需要 Bash、curl、jq 1.6+ 和系统基础命令（date、od、tr、sleep）。Linux/macOS 可直接使用 Bash，Windows 使用 Git Bash 或 WSL；所有脚本显式通过 `bash` 调用，无需修改执行权限：

```bash
bash scripts/demo.sh --scenario all
bash scripts/demo.sh --scenario pay
bash scripts/demo.sh --scenario timeout
# 额外覆盖尚无支付单时的关闭标记
bash scripts/demo.sh --scenario timeout --without-payment
# 可选启动容器并初始化，但仍需引擎已运行
bash scripts/demo.sh --start-stack --scenario all

bash scripts/check.sh --with-mq
# 终态收敛、业务静止后独立核对数据库事实
docker compose run --rm diagnose
```

demo 只读取 .env 的 DEMO_PASSWORD，不执行其中的命令；也支持 `--password` / DEMO_PASSWORD。URL 支持 API_BASE_URL、USER_BASE_URL、PAYMENT_BASE_URL，或 `--api-base-url`、`--user-base-url`、`--payment-base-url`。check 还支持 CATALOG_BASE_URL。脚本参数可用 `--help` 查看，不会访问服务。每次创建新活动：user01/SKU1 演示支付，user02/SKU2 演示超时，各额度 1。重复演示会保留新活动并消耗真实库存。

脚本覆盖鉴权、他人订单/支付访问、重复受理和参数冲突、重复支付单/确认、支付库存确认、到期释放与资格不恢复。超时路径按真实 expiresAt 等待，轮询超时不改变业务状态。diagnose 检查库存非负/守恒/流水、订单与支付/预占终态、订单唯一、资格与额度、未完成受理和任务。跨服务没有全局读快照，须在静止窗口执行。

## 地址与健康检查

| 服务 | HTTP | RPC / 依赖端口 |
| --- | --- | --- |
| user-service | localhost:18080 | — |
| catalog-service | localhost:18081 | localhost:19000 |
| trade-service | localhost:18082 | 调用库存、支付 |
| payment-service | localhost:18083 | localhost:19001 |
| MySQL / Redis | — | localhost:13306 / 16379 |
| RocketMQ | — | localhost:19876（NameServer）、10911（Broker） |

映射只绑定宿主机回环地址，RPC 不应公开外网。容器之间使用服务 DNS；ClientConn 长期复用并使用 round_robin，业务 RPC 截止 1 秒。

/livez 只判断 HTTP 进程；/readyz 检查所属库和使用中的依赖，失败返回 503。MQ TCP 探针不代表消费进度健康。Catalog/Payment 的 gRPC Health 表示 RPC 监听已启动，实际依赖以其 HTTP /readyz 为准。退出时撤销就绪，停止后台领取并排空 HTTP/gRPC，共用 15 秒预算，Compose 宽限 20 秒；退出重投仍待运行验收。

## 本地构建与 IDE

```bash
go mod download
go build ./...
go vet ./...
# Linux 容器目标交叉编译，只编译不运行服务
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

默认 Broker 公告 rmq-broker，仅供容器网络使用。Docker Desktop IDE 调试需覆盖配置，使宿主机与容器都能访问公告地址：

```bash
docker compose -f compose.yaml -f compose.debug.yaml up -d
docker compose stop trade-service
source configs/local.example.sh
go run ./cmd/trade
```

覆盖配置公告 host.docker.internal，本机与容器都必须能解析该名称，此配置尚未实测。其他环境需提供两边可访问的宿主机 DNS 和映射端口，不能只改 NameServer 或写死实例 IP。调试其他服务时改用所属库账号、不同 HTTP 端口，catalog/payment 分别设置 RPC_ADDR=:19000/:19001 并停止对应容器。

## 协议生成与排障

安装 [protoc 29.3](https://github.com/protocolbuffers/protobuf/releases/tag/v29.3)：

```bash
bash scripts/generate.sh --protoc /path/to/protoc
# Windows Git Bash 示例，路径按本机安装位置调整
bash scripts/generate.sh --protoc '/c/tools/protoc/bin/protoc.exe'
```

插件固定安装到忽略跟踪的 bin/，输出 api/gen/。本次新增 GetSKU 的 stock_available=5，未修改既有字段编号；删除字段必须 reserved。

- 引擎未运行时启动命令会失败，何时启动由使用者决定，本次不代为操作。
- 迁移失败看 migrate-* 日志；MySQL DDL 不保证整份文件回滚，不盲目 force 或删卷。
- MQ 故障看 mq-init、Broker、NameServer 日志，检查 Topic 和公告 DNS。
- 缺少 JWT_SECRET 会在监听前失败；依赖错误可在结构化日志中按 traceId/orderNo/eventId 追踪。
- Outbox/Inbox/operation_task 达到重试阈值后保留 NEEDS_REVIEW；运营通过 `/api/admin/reliability/{queue}` 查看，修复原因后 `POST /{id}/retry` 恢复。

M2 双实例缓存、并发、独立限流和 40 组故障回归的准备步骤及 Bash 命令见 [M2.md](docs/explain/M2.md)。`scripts/acceptance.sh` 只执行验收客户端，不负责启动任何服务。

既有 Vue API/Java 契约在工作区不可得，当前按 PRD 交付接口，前端兼容映射和 CORS 来源待验证。不新增前端，不默认开放任意来源。PRD 沿用本地被忽略的 docs/prd/。
