# dingdong-go 项目阅读指南

更新于 2026-10-26，按当前 M1+M2 工作区源码核对。先读本页，再选择需要的章节；不必从头读完所有文件。本文档组只解释当前实现，不把代码完成、编译成功和真实运行验收混为一谈。

## 1. 先知道这个项目要解决什么

这是限量活动商城后端。运营创建并发布活动，用户提交参与请求，系统异步预占 SKU 库存并创建订单；用户可以模拟支付，未付订单到期后关闭并释放库存。用户在同一活动最多形成一张订单，订单取消也不会获得再次参与资格。

系统由 user、catalog、trade、payment 四个独立进程组成。各服务有自己的数据库和职责；HTTP 面向用户/运营，gRPC 用于同步内部操作，MQ 加持久化任务用于异步推进。

M1 建立购买业务闭环，M2 在同一业务规则上增加缓存、流量保护、准入恢复、持久化消费与有界重试。它们不是两套可以在当前目录切换运行的代码。

## 2. 按什么顺序读

| 顺序 | 文档 | 看完应能回答 |
| --- | --- | --- |
| 1 | [框架.md](框架.md) | 每个目录负责什么？入口如何装配？业务与平台怎样分工？ |
| 2 | [M1业务流程.md](M1业务流程.md) | 从受理到成单、支付、关闭，各阶段落什么事实？失败后如何继续？ |
| 3 | [grpc.md](grpc.md) | proto 如何生成类型和代理？RPC 如何注册、调用和处理不确定结果？ |
| 4 | [M2.md](M2.md) | 缓存、限流、预扣、代次、Inbox、任务租约分别防什么问题？ |
| 5 | [API.md](../../API.md) | 实际请求路径、字段、状态和运营接口是什么？ |
| 6 | [README.md](../../README.md) | 环境怎样启动？迁移、种子、调试和脚本怎样使用？ |

只想先读懂成功路径：M1 第 1～8 节 → order.go 的 createOrder、createPayment、paymentSucceeded → catalog/inventory.go 和 payment/payment.go。

只想读懂当前提交接口：trade/http.go 的请求路由 → rate.go 的 requestLimit → request.go 的 submit/persistTicket → admission.go 的 prehold → lua/admit.lua。M2 文档按这个顺序解释。

只想理解异步可靠性：event.Put → event.Bus → event_inbox → trade.handleEvent → work.PutTask → work.Queue → trade.handleTask。M2 里有这条链的分层图。

## 3. 必须先分清的词

| 名称 | 所在位置 | 它表达什么 | 它不代表什么 |
| --- | --- | --- | --- |
| 活动额度 quota | trade.activity | 活动允许的参与数量 | 全局 SKU 真实库存 |
| used_quota | trade.activity | 已经持久化受理且尚未退回的额度事实 | 当前未付款订单数 |
| remaining | Redis 准入 config | 当前代次可继续预扣的活动额度 | 可以无条件写入 SQL 的授权 |
| 预扣 ticket | Redis records/pending | 临时占活动额度和用户资格 | 已经受理或已预占 SKU |
| 受理 request | trade.activity_request | 某用户某 requestId 的持久化结果 | orders 表一定已有记录 |
| 预占 reservation | catalog.inventory_reservation | 真实 SKU 库存锁定/确认/释放事实 | 活动资格状态 |
| 支付单 payment | payment.payment | 模拟支付的持久化事实 | 交易订单已完成库存确认 |
| Outbox | 发送方数据库 | 下一步事件已与业务一同提交 | 对端已处理完成 |
| Inbox | 接收方数据库 | 收到的消息已落库，可交给本地继续处理 | 订单业务已完成 |
| operation_task | trade 库 | 某业务键某动作的可靠待办 | 每轮扫描创建一个新订单 |
| NEEDS_REVIEW | 持久化队列 | 自动尝试已停止，待排查/显式恢复 | 业务请求一定 FAILED |

不要把 Redis 活动预扣与 catalog SKU 预占都叫作“扣库存”；它们属于不同服务、不同事实和不同恢复策略。

## 4. M1 与 M2 的对应关系

| 能力 | M1 阶段 | 当前 M2 演进 |
| --- | --- | --- |
| 商品、订单、支付状态规则 | 已有 | 继续复用，不因缓存或重试改变 |
| 新请求准入 | 直接走 MySQL 准入事务，Redis 做探测 | 重放 → 两层限流 → Lua 预扣 → SQL 最终确认 |
| 公共活动详情 | 数据库查询 | Redis+随机 TTL+空值缓存+singleflight+回源预算 |
| 发送事件 | 最小 Outbox | 统一租约队列、有限退避、人工恢复 |
| 消费事件 | SDK 回调直接推进业务 | SDK 先落 Inbox，Inbox 再落任务，任务执行业务 |
| 扫描恢复 | 扫描直接尝试业务动作 | 扫描只补齐任务，由队列统一调度 |
| Redis 丢失 | 没有准入镜像恢复问题 | SQL epoch/revision、Redis builder/ready、分批重建 |
| 验证工具 | demo、mqcheck、diagnose | 增加双实例缓存、500 用户、独立限流、故障/关键窗口验收 |

阅读 M1 文档时，关注业务为什么这样转移；阅读 M2 文档时，关注增加了哪些持久化边界和竞争条件。文档中的历史说明会明确标成 M1 阶段，当前执行链路以 M2 为准。

## 5. 从一个现象反查源码

| 观察到的现象 | 先读哪里 |
| --- | --- |
| HTTP 202 但查不到订单 | [request.go](../../internal/trade/request.go)、[order.go](../../internal/trade/order.go) |
| 429，message 是 USER_RATE_LIMITED/ACTIVITY_RATE_LIMITED | [rate.go](../../internal/trade/rate.go)、[rate.lua](../../internal/trade/lua/rate.lua) |
| 返回准入重建中或旧请求失败 | [admission.go](../../internal/trade/admission.go)、[request.go](../../internal/trade/request.go) |
| 标题刚改完还看到旧值 | [cache.go](../../internal/trade/cache.go)、[activity.go](../../internal/trade/activity.go) |
| 支付成功但暂时不能履约 | [order.go](../../internal/trade/order.go) 的 finishOrder/settleInventory |
| 订单停在 CLOSING | [order.go](../../internal/trade/order.go) 的 closeOrder，及 payment 查询/关闭 |
| MQ 已确认但业务没完成 | [event/bus.go](../../internal/platform/event/bus.go)、[worker.go](../../internal/trade/worker.go) |
| 队列进入 NEEDS_REVIEW | [work/queue.go](../../internal/platform/work/queue.go)、[work/http.go](../../internal/platform/work/http.go) |
| Redis 重启后额度没恢复为满额 | [admission.go](../../internal/trade/admission.go) 的 buildAdmission |
| 服务重启后指标归零 | [metrics.go](../../internal/platform/metrics/metrics.go)，指标属于进程内存 |

## 6. 阅读时做什么笔记最有用

对每个业务方法，按下面四个问题读，而不是逐行记语法：

1. 输入来自哪里，哪些字段可信？例如用户 ID 来自验签上下文，金额来自订单快照。
2. 本次事务修改哪些表？哪些网络动作在事务外？
3. 成功与失败分别留下什么可恢复证据？例如 ACCEPTED、FAILED、Outbox 或任务。
4. 重复调用、超时、进程退出、旧执行者回来时，会读到什么既有事实？

几个稳定关联键需要一起记：userId+requestId 表达 HTTP 幂等；activityId+userId 表达活动资格；orderNo+skuId 表达预占；orderNo 表达支付单；eventId 表达消息去重；business_key+action 表达任务去重。

## 7. 文档与验证边界

命令均使用 Bash。普通脚本用双横线参数，例如 `bash scripts/demo.sh --scenario pay`；acceptance.sh 透传 Go flag，使用 `-mode` 等单横线参数。现有脚本和本次讲解都不应被误读成已运行报告。

当前 README 记载 M1/M2 业务代码和验收工具已实现，运行验收尚未执行；M3 未实现。本次只核对源码和文档，没有运行 Docker、迁移、交易或故障注入，也不生成 QPS、恢复时限或通过率结论。

参数如“一秒 RPC、十五秒租约、三十秒验收等待”分别是调用预算、领取保护期限和客户端判定上限，不是性能承诺。指标 sum/count 得到平均值，也不能当作 P95/P99。

docs/ 沿用仓库本地忽略规则。若后续源码继续演进，应同时更新本索引、阶段对照、关键默认值和相关调用链。
