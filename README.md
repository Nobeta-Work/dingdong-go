# DingDong Go

限量活动商城后端 Demo。一个 Go module，四个独立进程，业务数据由各服务独立拥有。

**当前进度：M1 基础框架。尚未完成 M1 可运行交易闭环。** 没有业务成功桩，也没有前端代码。

## 目录与阅读顺序

```text
cmd/                       四服务入口、seed、mqcheck、healthcheck 工具
internal/
  user/                    用户服务启动，后续实现账号、角色和登录
  catalog/                 商品服务启动，后续实现 SKU、库存本地事务
  trade/                   交易服务启动，后续实现活动、受理、订单与编排
  payment/                 支付服务启动，后续实现模拟支付与支付事件
  platform/
    config/                环境变量读取与基础配置校验
    app/                   连接池、HTTP/gRPC、依赖检查和退出排空
    httpx/                 Gin、统一响应、请求追踪与结构化日志
    event/                 MQ 事件信封
api/proto/                 Catalog、Inventory、Payment 协议源文件
api/gen/                   通过脚本生成的 Go 协议代码
migrations/{service}/      各库独立、带版本的增量迁移
deploy/                    MySQL 首次建库授权、RocketMQ 配置
configs/                   IDE 调试配置示例
scripts/                   协议生成、真实框架接口检查
compose.yaml               四服务和依赖，不含前端或自研网关
```

推荐先读 `cmd/trade/main.go` → `internal/trade/service.go` → `internal/platform/app/app.go`，理解入口、服务装配和连接生命周期；再读 `httpx`、迁移和 RPC 契约。

HTTP Handler、RPC Handler 和消费者后续放在各自服务包中，共用该服务业务层。当前不为尚未实现的业务创建空的 handler/service/repository 层。公共平台包不能写业务表，禁止服务间跨库 JOIN 或直接修改对方数据。

## 固定版本

| 组件 | 版本与选择 |
| --- | --- |
| Go | 1.27.1，与已有 go.mod 一致；镜像 golang:1.27.1-bookworm |
| Gin | v1.10.0 |
| grpc-go / Protobuf Go | v1.70.0 / v1.36.5 |
| protoc / Go gRPC 生成器 | 29.3 / v1.5.1 |
| MySQL 驱动 / 服务端 | v1.9.0 / mysql:8.4.4 |
| go-redis / Redis | v9.7.3 / redis:7.4.2-alpine |
| RocketMQ / Go SDK | apache/rocketmq:4.9.8 / rocketmq-client-go/v2 v2.1.2 |
| 迁移工具 | migrate/migrate:v4.18.2 |

采用 RocketMQ 4.x NameServer + Broker 及经典 Go v2 客户端，不需要 Proxy。重投检查使用 Broker 延时级别 1（1 秒），没有使用 5.x 任意时间戳延时接口。业务延时事件和扫描任务尚未实现。依据：[官方 Compose 说明](https://rocketmq.apache.org/docs/4.x/quickstart/03quickstartWithDockercompose/)、[固定 SDK 重投示例](https://github.com/apache/rocketmq-client-go/blob/v2.1.2/examples/consumer/retry/concurrent/main.go)。版本组合仍需在本机 Docker 环境实测。

## 启动基础框架

需要 Docker Desktop 的 Linux 容器引擎与 Compose v2。建议预留 4 CPU、6 GB 内存；这是启动预算建议，尚无实际容器资源测量结果。

以下 PowerShell 命令在仓库根目录执行：

```powershell
Copy-Item .env.example .env
docker compose config --quiet
docker compose up -d --build
docker compose ps -a
```

复制配置只需首次执行。若默认 Go 代理不可达，在 `.env` 将 `GOPROXY` 改为 `https://goproxy.cn,direct` 后构建。Docker 镜像下载使用 Docker 自身的镜像源设置。

MySQL 空数据卷首次执行 `deploy/mysql/init/00-databases.sql`，创建四个逻辑库、四个仅有所属库 DML 权限的应用账号和四个库级迁移账号。随后每个迁移容器执行 `up`，服务等待所属库迁移成功再启动。增量变更应新增迁移文件，不修改已经执行的版本。

Demo 数据库账号密码见 Compose 和建库 SQL，仅用于本地开发。应用不使用 root。`.env` 的 root 密码只在首次创建 MySQL 卷时生效，修改文件不会自动修改已有库密码。MySQL 初始化脚本修改后也不会自动作用于旧卷。

```powershell
docker compose run --rm seed-user
docker compose run --rm seed-catalog
.\scripts\check.ps1 -WithMQ
```

种子账号为 `user01`、`user02`、`operator` 和 `bench0001`～`bench0500`，共 503 个；初始密码来自 `.env` 的 `DEMO_PASSWORD`，bcrypt 哈希存储。目录内的账号只用于后续业务实现，**当前没有登录接口**。商品种子为两个独立 SKU，每个初始库存 200，金额 1990 分。重复初始化不覆盖密码、角色、商品或库存。尚未种子初始化活动，避免绕过未来的发布流程与 SKU RPC 校验。

`check.ps1` 检查真实 `/livez`、`/readyz` 及 traceId；`-WithMQ` 再调用真实 SDK 发送消息，首次消费主动返回重试，只有收到 `ReconsumeTimes > 0` 的 Broker 重投才通过。成功日志为 `mq_check_passed`。这只验证基础设施，不是 PRD A01～A06 业务验收。

```powershell
# Linux/macOS 也可直接检查与运行工具，不依赖 PowerShell。
curl --fail http://localhost:18082/readyz
docker compose run --rm mqcheck
```

正常停止使用 `docker compose down`，**保留所有数据卷**。本项目的启动和初始化命令不删除数据，也不重置活动额度。只有显式执行 `docker compose down -v` 才会删除本项目数据卷和其中全部数据；该命令不属于正常启动步骤。

## 端口与健康检查

| 服务 | HTTP | RPC / 依赖端口 |
| --- | --- | --- |
| user-service | localhost:18080 | 无业务 RPC |
| catalog-service | localhost:18081 | localhost:19000 |
| trade-service | localhost:18082 | 调用 catalog/payment |
| payment-service | localhost:18083 | localhost:19001 |
| MySQL | — | localhost:13306 |
| Redis | — | localhost:16379 |
| RocketMQ | — | localhost:19876（NameServer）、10911（Broker 探测） |

所有宿主机映射只绑定 `127.0.0.1`。RPC 在容器中使用服务 DNS，不写死 IP；RPC 仅用于内部调用，当前无 TLS/鉴权，不应公开到互联网。

`GET /livez` 只判断进程响应；`GET /readyz` 检查当前服务使用的依赖，失败返回 503，避免把依赖故障当作进程死亡。数据库检查执行 Ping，Redis 执行 PING，交易服务调用库存和支付的标准 gRPC Health Check。MQ 这里只探测 NameServer/Broker TCP 连通性，不验证 Topic 或消费进度；通过 mqcheck 单独验证消息能力。

Catalog/Payment 的 gRPC 健康服务当前表示 RPC 进程已经启动，依赖状态以其 HTTP `/readyz` 为准。业务 RPC 契约已定义，但没有注册业务处理器，调用返回 gRPC `Unimplemented`。参考：[标准 gRPC 健康协议](https://grpc.io/docs/guides/health-checking/)。

统一 HTTP 响应：

```json
{"code":"OK","message":"基础设施就绪","data":{"service":"trade-service","stage":"M1_FRAMEWORK"},"traceId":"..."}
```

当前错误码：`NOT_FOUND`（404）、`METHOD_NOT_ALLOWED`（405）、`DEPENDENCY_UNAVAILABLE`/`NOT_READY`（503）、`INTERNAL_ERROR`（500）。后续业务错误码按 PRD 在实际接口中加入，不将未实现功能返回为成功。

## 本地 Go 与 IDE 调试

```powershell
go mod download
go build ./...
go vet ./...
```

无需单独测试模块。框架验证使用编译、静态检查、真实 HTTP 和 MQ 工具。

例如调试交易服务：先按上文启动全部容器，再停止其容器以腾出端口，保持上下游运行：

```powershell
docker compose stop trade-service
. .\configs\local.example.ps1
go run ./cmd/trade
```

其他服务修改 `MYSQL_DSN` 为各自账号与库，设置不同的 `HTTP_ADDR`；catalog/payment 还需分别设置 `RPC_ADDR=:19000`/`:19001`，并停止对应容器。不要让应用账号访问其他服务库。

Broker 公布容器 DNS `rmq-broker`，宿主机不能直接使用该地址完成 MQ 业务通信。框架阶段宿主机仅做 Broker TCP 检查；真实消息工具通过 `docker compose run --rm mqcheck` 在网络内部运行。后续 IDE 调试 MQ 消费者需配置宿主机和容器都可解析、访问的 Broker 公告地址，不能只映射 NameServer 端口。

## Protobuf 生成

安装 [protoc 29.3](https://github.com/protocolbuffers/protobuf/releases/tag/v29.3)，执行：

```powershell
.\scripts\generate.ps1 -Protoc 'C:\tools\protoc\bin\protoc.exe'
```

脚本将固定版本的两个 Go 插件安装到忽略跟踪的 `bin/`，输出 `api/gen/`。生成文件提交版本控制，普通构建无需 protoc。已有字段编号不得变更；删除字段使用 `reserved`。

## 排障

- Docker 提示找不到 Linux Engine 管道：启动 Docker Desktop 并确认 Linux 引擎可用。
- `docker compose ps -a` 出现迁移非零退出：查看 `docker compose logs migrate-trade`（或对应服务），先修复迁移。不要对未知失败盲目执行 `force`，MySQL DDL 不能保证整份文件回滚。
- 服务反复重启：查看 `docker compose logs trade-service`，启动错误会列出失败依赖名称，不输出 DSN 凭据。依赖调用有超时，服务不会无限等待。
- MQ 初始化失败：查看 `docker compose logs mq-init rmq-broker rmq-nameserver`，确认 Broker 内存和服务 DNS；修复后执行 `docker compose up -d mq-init trade-service payment-service`。
- MQ 重投检查超时：确认 Topic 已创建、Broker 公告地址可达；检查工具必须在 Compose 网络运行。每次检查会产生独立临时消费组和消息，只适合 Demo 验证。
- SIGTERM 退出：先撤销就绪状态，再排空 HTTP/gRPC，预算 15 秒，Compose 宽限期 20 秒；尚未运行业务消费者，不宣称已验证消息排空。

## 阶段进度与验证记录

2026-10-24，Windows，本机 Go 1.27.1 windows/386、Docker CLI 29.5.2；Docker Linux 引擎未启动。

已实际通过：

- `go build ./...`：本机 Windows 编译。
- `go vet ./...`：静态检查。
- `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...`：Linux 容器目标交叉编译（PowerShell 使用 `$env:` 设置变量）。
- protoc 29.3 + 固定 Go 插件生成 `api/gen/`，生成代码参与上述编译。
- `docker compose --env-file .env.example config --quiet`：Compose 配置解析与依赖关系校验。
- PowerShell Parser 检查全部配置与脚本语法；`gofmt` 与 `git diff --check` 检查通过。

未执行：镜像构建和拉取、容器启动、迁移 SQL 的真实执行、种子重复初始化、HTTP 运行检查、MQ 生产/消费/重投及退出排空。这些检查需要可用的 Docker Linux 引擎；不能用交叉编译或 Compose 配置通过代替运行结论。

| 阶段 | 已实现 | 未完成 |
| --- | --- | --- |
| M1 框架 | 四入口、Gin、配置、日志、统一响应、健康检查、独立库账号、数据库结构与迁移、种子工具、gRPC 健康通信及业务协议、Compose、MQ 检查工具 | 容器运行验证；JWT 登录、活动管理/查询、受理、真实库存 RPC、异步成单、模拟支付、超时关单、业务演示与不变量脚本 |
| M2 | 仅预留 Outbox、任务表及事件信封 | 缓存、singleflight、Lua 限流/额度、Outbox 投递和任务领取、补偿、故障注入与并发验收 |
| M3 | 仅基础退出生命周期可复用 | K8s/Traefik 清单、双副本、探针和资源实测、滚动更新验收 |

PRD 在本地 `docs/prd/`，该目录沿用仓库原有忽略规则。未找到既有 Vue 的 API 封装或 Java 契约，接口兼容映射、CORS 来源和前端联调均待验证，不默认放开任意来源。

后续按纵向业务实现：登录 → SKU/库存 RPC → 活动与持久化受理 → MQ 成单 → 支付成功及库存确认 → 到期关闭及释放。M1 应保留真实数据库与消息链路；M2 再补齐完整故障恢复和并发保护，不能用内存订单或固定成功库存接口替代。
