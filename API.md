# M1 接口与状态契约

本文描述已实现的接口，不代表完成运行验收。默认 user/catalog/trade/payment HTTP 端口为 18080/18081/18082/18083。

## 通用约定

统一响应包含 code、message、data、traceId，响应头带 X-Trace-ID。成功 code=OK；登录后发送 `Authorization: Bearer <accessToken>`。金额单位为人民币分，业务 ID 为字符串，时间为带时区 ISO 8601，服务读写 UTC。

列表接收 page（默认 1）、pageSize（默认 20，最大 100），返回 items、total、page、pageSize。请求 ID 允许 1～64 位字母、数字、下划线或短横线，推荐 UUID。数量固定 1。身份只取验签上下文，客户端 userId 不决定归属。

| HTTP | code | 含义 |
| --- | --- | --- |
| 200/201/202 | OK | 查询或操作成功/草稿创建/新受理 |
| 400 | INVALID_ARGUMENT | 字段、ID、分页或业务参数错误 |
| 401 | UNAUTHORIZED | 登录或令牌无效 |
| 403 | FORBIDDEN | 无运营权限、访问他人订单/支付 |
| 404 | NOT_FOUND | 对象或接口不存在；请求查询按当前用户过滤 |
| 405 | METHOD_NOT_ALLOWED | 不支持的方法 |
| 409 | CONFLICT | 请求参数冲突、资格已占用、额度耗尽、状态不允许等 |
| 503 | DEPENDENCY_UNAVAILABLE/NOT_READY | 依赖失败、未知结果、退出中 |
| 500 | INTERNAL_ERROR | HTTP 未预期 panic |

M1 未实现令牌桶，不产生业务限流 429。技术错误不会直接把未知结果写成 FAILED。业务受理终态和 failureReason 持久化保留。

## 用户与商品

| 服务 | 方法与路径 | 权限与行为 |
| --- | --- | --- |
| user | POST /api/auth/login | body: username、password；返回 accessToken、expiresAt、user（id/username/role） |
| user | GET /api/auth/me | 登录，查询当前用户 |
| catalog | GET /api/skus/{id} | 公开，返回 skuId/name/priceCent/available（商品启用状态） |
| catalog | GET /api/admin/skus/{id}/inventory | 运营，返回 skuId/total/available/locked/sold |

JWT 使用 HS256，issuer=dingdong，有效期两小时；角色为 USER 或 OPERATOR，密码由 seed 以 bcrypt 存储。库存变更仅经内部 RPC。

## 交易服务

| 方法与路径 | 权限 | 行为 |
| --- | --- | --- |
| GET /api/activities | 无 | 分页列出 PUBLISHED/PAUSED 活动 |
| GET /api/activities/{id} | 无 | 公共详情，草稿/发布中返回 404 |
| GET /api/activities/{id}/participation | 登录 | 当前受理或最近失败请求，无历史返回 AVAILABLE |
| POST /api/activities/{id}/requests | 登录 | body: requestId；新受理 202，同参数重放 200 |
| GET /api/requests/{requestId} | 登录 | 当前用户受理结果，跨用户返回不存在 |
| GET /api/orders | 登录 | 本人订单，分页并可按 activityId 过滤 |
| GET /api/orders/{orderNo} | 登录 | 本人订单详情 |
| POST /api/orders/{orderNo}/payment | 登录 | 创建/获取支付单；金额、用户和截止时间来自订单 |
| POST /api/admin/activities | 运营 | 新建草稿，返回 201 |
| POST /api/admin/activities/{id}/publish | 运营 | 幂等发布，不重置额度 |
| POST /api/admin/activities/{id}/pause | 运营 | 暂停新受理，既有受理继续执行 |
| GET /api/admin/activities/{id}/stats | 运营 | asOf、额度、分状态的受理与订单计数 |
| GET /api/admin/orders | 运营 | 全部订单，分页并支持 activityId 过滤 |

创建草稿的 JSON 字段为 title、skuId、priceCent、quota、startsAt、endsAt；例如：

```json
{"title":"演示活动","skuId":"1","priceCent":1990,"quota":1,"startsAt":"2026-10-25T00:00:00Z","endsAt":"2026-10-26T00:00:00Z"}
```

实际调用设置当前有效时间。时间必须有序，发布时额度不能超过 SKU 可用库存；已发布交易字段没有修改接口。

活动字段：id、title、skuId、skuName、priceCent、quota、usedQuota、startsAt、endsAt、version、status、phase、participationRule。phase 按时间计算为 UPCOMING/ACTIVE/ENDED，需结合发布状态判断是否可参与。

受理字段：requestId、userId、activityId、orderNo、status、failureReason。orderNo 在受理时固定分配，**不代表已经生成订单**。客户端区分 ACCEPTED/PROCESSING/ORDER_CREATED/FAILED。参与查询可额外返回 qualificationStatus。

同一用户同 requestId 换活动参数为 409；已有受理/订单时换新 requestId 也不能重复成单。成单前最终失败可退回资格和额度，旧失败请求保持原结果。订单生成后资格消耗，即使取消也不恢复。

订单字段：orderNo、activityId、userId、amountCent、expiresAt、createdAt、status、inventoryStatus、fulfillable、skuId、skuName、priceCent、quantity。商品信息来自受理快照。只有 PAID + CONFIRMED 的 fulfillable=true。

支付单创建返回 paymentNo、orderNo、userId、amountCent、expiresAt、status、simulated。客户端不能指定金额或延长窗口。

## 支付服务

| 方法与路径 | 权限与行为 |
| --- | --- |
| GET /api/payments/{paymentNo} | 登录且归属本人，返回支付字段与 simulated=true |
| POST /api/payments/{paymentNo}/simulate-success | 登录且归属本人；未关闭且未过期才能首次确认，重复成功幂等 |

模拟成功不产生真实扣款。支付响应成功不代表交易和库存已确认，需要继续查询订单。

## 内部 RPC

| 服务与方法 | 约束 |
| --- | --- |
| Catalog.GetSKU | skuId；返回 stock_available 供发布校验 |
| Inventory.Reserve | orderNo+skuId，quantity=1，原子预占 |
| Inventory.GetReservation | 按同一业务键回查 |
| Inventory.Confirm | LOCKED → CONFIRMED，重复不二次扣减 |
| Inventory.Release | LOCKED → RELEASED；不存在时先写终止标记 |
| Payment.CreateOrGet | orderNo 唯一，校验用户、金额、截止时间一致；尊重 CLOSED 标记 |
| Payment.QueryByOrder | 不存在为 NotFound，与技术故障分开 |
| Payment.CloseByOrder | 已成功返回 SUCCESS；未到期 PENDING 拒绝；不存在则写 CLOSED 标记 |

RPC 明确区分 InvalidArgument、NotFound、FailedPrecondition、ResourceExhausted（SKU_STOCK_UNAVAILABLE）、Unavailable、DeadlineExceeded。未知写入结果按业务键回查，不当作缺货或关闭成功。

## 状态与事件

- 活动：DRAFT → PUBLISHING → PUBLISHED → PAUSED。
- 受理：ACCEPTED → PROCESSING → ORDER_CREATED / FAILED。
- 资格：AVAILABLE → HELD → CONSUMED；仅成单前最终失败可退回，变更校验 holderRequestId。
- 订单：PENDING_PAYMENT → PAID，或 PENDING_PAYMENT → CLOSING → PAID / CANCELED。
- 预占：LOCKED → CONFIRMED / RELEASED；允许先落 RELEASED 标记。
- 支付：PENDING → SUCCESS / CLOSED；允许先落 CLOSED 标记。

真实 MQ Topic 为 dingdong-events，类型包含 CreateOrderRequested、PaymentSucceeded、InventoryConfirmRequested、InventoryReleaseRequested、OrderExpiryDue。信封包含 eventId/eventType/aggregateId/occurredAt/schemaVersion/payload/traceId。

M1 到期提醒由 Outbox 持久化排期投递，每五秒分页扫描作为兜底；受理超过十秒未完成可恢复处理。扫描与轮询间隔不改变 expiresAt。

## 前端兼容

工作区没有既有 Vue API 封装或 Java 契约，当前按 PRD 路径、Bearer 头、统一响应和上述枚举交付。旧分页、错误码、状态枚举、代理及 CORS 来源的兼容映射待资料补齐后验证，不宣称联调完成，不新增前端。
