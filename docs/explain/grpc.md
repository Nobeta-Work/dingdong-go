# gRPC / Protobuf：从原理到 dingdong-go 实操

本文面向已经接触 Go，但尚不熟悉 RPC 的读者。目标是读完后能理解项目的 `.proto`、生成代码、服务端注册和客户端调用，并独立跑通一个 RPC。

本文命令统一使用 Bash 语法，在仓库根目录执行；Windows 可使用 Git Bash 或 WSL。使用 WSL 时，Go、protoc 等工具应安装在相应 Linux 环境中。

依据：2026-10-26 的 M1+M2 工作区源码；Go 1.27.1、grpc-go v1.70.0、Protobuf Go v1.36.5、protoc 29.3、protoc-gen-go-grpc v1.5.1。示例按这组版本编写，不要求升级依赖。第 8 节是独立练习，第 7、9、12 节讲解现有真实业务实现。M1 业务与 M2 可靠性机制已实现，运行验收仍以 README 的记录为准；本次只更新文档。完整阅读路线见 [项目阅读指南](README.md)，准入与队列见 [M2](M2.md)。

## 阅读路线

- 第 1～4 节：理解 RPC、gRPC、Protobuf，以及一次调用到底发生了什么。
- 第 5～7 节：对照项目理解协议、生成代码和当前实现边界。
- 第 8 节：复制两个完整程序，运行第一个真实网络 RPC，无须数据库和 Docker。
- 第 9～12 节：学习超时、错误、重试、中间件与真实项目接入。
- 第 13～15 节：了解流式 RPC、排障方法和后续练习。

## 1. 为什么需要 RPC

### 1.1 从普通函数调用说起

假设交易服务需要商品价格。如果商品查询和交易逻辑在同一个进程内，可以调用一个普通 Go 函数：

```go
sku, err := catalog.GetSKU(ctx, skuID)
```

但本项目中交易服务与商品服务是两个独立进程，交易服务不能直接访问商品服务的内存。即便导入了同一个 Go 包，也只是把代码编译到当前程序，并不会因此调用远处正在运行的服务。

要让商品服务实际执行查询，需要把请求发过去，让对方处理后再传回结果。RPC（Remote Procedure Call，远程过程调用）就是对这类过程的封装。

调用形式可以仍然接近函数：

```go
sku, err := client.GetSKU(ctx, &catalogv1.GetSKURequest{SkuId: "1"})
```

区别在于 `client` 是客户端代理，这一行背后有序列化、网络传输、远程执行和结果解码。**语法像本地调用，故障模型属于网络调用。**

本地函数返回失败时通常可以知道执行到哪一步；远程写操作超时，则可能是“没收到请求”，也可能是“已成功但响应丢失”。后面讨论超时和幂等时会反复用到这个区别。

### 1.2 为什么不直接读商品数据库

在本项目中，商品服务拥有 SKU 和库存数据。交易服务直接查询商品库，会绕过商品服务的权限、校验和数据演进边界，还会迫使交易服务了解对方表结构。

使用 RPC 后，交易服务只依赖“查询 SKU”这个契约。商品服务可以在内部调整查询、缓存和表结构，只要维持对外协议的兼容性。

这不表示所有 Go 项目都需要 RPC。如果两个模块本来就在同一个进程内，普通函数或接口通常更直接；引入 RPC 会增加部署、超时、监控和故障处理成本。

## 2. gRPC、Protobuf、HTTP/2 分别是什么

| 名称 | 所处职责 | 本项目中的例子 |
| --- | --- | --- |
| RPC | 一种跨进程调用方式 | trade 请求 catalog 查询 SKU |
| gRPC | RPC 框架，负责方法调用与传输等机制 | `grpc.NewServer`、`grpc.NewClient` |
| Protobuf | 接口描述语言及结构化数据序列化体系 | `.proto`、生成消息、二进制编码 |
| HTTP/2 | 原生 gRPC 使用的传输协议基础 | 在连接上承载多个 RPC stream |
| TCP / TLS | 更底层的连接与安全传输 | 本地明文 TCP；正式环境可配置 TLS |

默认组合可以画成：

```text
业务方法：GetSKU
    ↓ 使用生成的客户端/服务端接口
gRPC：方法名、状态、超时、metadata、调用生命周期
    ↓ 默认使用 Protobuf 编解码消息
HTTP/2：headers、DATA、trailers、多路复用与流控
    ↓
TCP；配置安全凭据后在 TCP 上使用 TLS
```

Protobuf 可以单独用于文件或消息编码，不要求使用 gRPC；gRPC 也支持其他 codec，但本项目使用默认 Protobuf。`.proto` 文件既可以定义消息，也可以定义 gRPC 服务方法。参见 [gRPC 核心概念](https://grpc.io/docs/what-is-grpc/core-concepts/)。

### 2.1 和常见 HTTP JSON 接口如何选择

下面是结合本项目需求的选型判断，不是速度排名：

| 需求 | 较自然的选择 | 原因 |
| --- | --- | --- |
| 浏览器调用商城公开接口 | HTTP JSON | 对浏览器、调试工具和常见前端开发流程直接友好 |
| trade 查询 SKU、请求库存预占 | gRPC | 内部强类型契约，调用方需要立即拿到结果 |
| 支付成功后通知交易服务处理 | MQ 事件 | 可以异步消费，需要考虑持久化、重投和去重 |
| 同一进程内调用订单规则 | Go 函数或接口 | 不必经过网络 |

gRPC 并不会自动实现业务幂等、分布式事务、服务发现平台或可靠事件投递。也不能简单理解为“一定比 JSON 快”：消息大小、并发方式、数据库和网络都影响结果。

普通浏览器 `fetch` 不直接实现原生 gRPC 协议；浏览器场景可采用 gRPC-Web 等适配方案。当前项目采用 HTTP 对外、gRPC 内部协作的方向，没有引入这类浏览器适配层。

## 3. Protobuf 怎样把消息变成字节

### 3.1 `.proto` 是约定，不是数据库表

项目中的请求定义是：

```proto
message GetSKURequest {
  string sku_id = 1;
}
```

可以分别理解为：

- `message`：定义一种网络消息结构。
- `string`：字段的数据类型。
- `sku_id`：协议中的字段名。
- `1`：字段编号，用于二进制编码标识字段。

`= 1` **不是默认值、数组下标，也不是参数顺序**。它是字段固定身份；调整声明顺序而保留各自编号不会改变字段对应关系，编号也不要求连续。在同一个 message 内，字段编号必须唯一。生成的 Go 类型中，对应字段名是 `SkuId`。

### 3.2 一个可以手算的编码例子

把 `sku_id` 设置为字符串 `"1"`，Protobuf 二进制编码为：

```text
0A 01 31
│  │  └─ 字符 '1' 的 UTF-8 字节
│  └──── 字符串字节长度：1
└─────── tag：(字段编号 1 << 3) | wire type 2 = 10 = 0x0A
```

wire type 2 表示 length-delimited，用于字符串、字节和嵌套消息等。整数常使用 varint，小整数可用较少字节；负数较多的整数场景可考虑 ZigZag 编码的 `sint32/sint64`。

这份二进制不包含 `sku_id` 字段名。解码端根据协议知道“编号 1 是字符串字段”，才能还原语义。Protobuf 不是压缩算法，也不是加密；不应把二进制看不懂当成数据安全措施。详见 [Protobuf 编码说明](https://protobuf.dev/programming-guides/encoding/)。

### 3.3 单独验证序列化

下面只是函数片段，需要导入 `fmt`、项目 `catalogv1` 包和 `google.golang.org/protobuf/proto`：

```go
req := &catalogv1.GetSKURequest{SkuId: "1"}
data, err := proto.Marshal(req)
if err != nil {
    return err
}
fmt.Printf("% X\n", data) // 输出 0A 01 31。

var decoded catalogv1.GetSKURequest
if err := proto.Unmarshal(data, &decoded); err != nil {
    return err
}
fmt.Println(decoded.GetSkuId()) // 输出 1。
```

正常 gRPC 调用已经自动完成这一步，不需要手动 Marshal/Unmarshal。这里单独展示是为了理解数据传输的本质。

## 4. 一次 gRPC 调用的完整过程

以 `trade → catalog.GetSKU` 的一元调用为例：

```text
trade 进程                                     catalog 进程
业务代码
  │ client.GetSKU(ctx, req)
  ▼
生成的 CatalogClient
  │ ClientConn.Invoke(完整方法名, req, out)
  ▼
gRPC 客户端运行时
  │ 解析地址、选择连接、编码消息
  ├──────── HTTP/2 stream ───────────────────→ gRPC 服务端运行时
  │                                            │ 根据方法名查找注册表
  │                                            ▼
  │                                         生成的 Handler
  │                                            │ 解码请求、执行 interceptor
  │                                            ▼
  │                                         自己实现的 GetSKU
  │                                            │ 查询业务层/数据库
  │                                            ▼
  │                                         响应消息或 gRPC 错误
  ◀──────── 响应字节与状态 ──────────────────────┤
  │ 解码成 *SKU 或返回 error
  ▼
业务代码继续执行
```

### 4.1 HTTP/2 上究竟传了什么

此方法的完整名字是：

```text
/dingdong.catalog.v1.Catalog/GetSKU
```

其 HTTP/2 请求使用 POST，`:path` 对应该名字，内容类型通常为 `application/grpc`，使用 Protobuf 时也可带 `+proto` 后缀。消息负载前有 5 字节 gRPC 消息头：1 字节压缩标记、4 字节大端消息长度，然后才是消息字节。

因此上节 3 字节请求在未压缩的 gRPC 消息负载中表示为：

```text
00 00 00 00 03 0A 01 31
│  └────────┘ └───────┘
│    长度 3    Protobuf 消息
└─ 未压缩
```

这还不包含 HTTP/2 帧头、headers 等开销。一条 gRPC 消息也不要求恰好对应一个 HTTP/2 DATA 帧。

最终 RPC 状态通过 `grpc-status` 表达，通常放在响应 trailers 中；HTTP 200 也可能携带非 OK 的 gRPC 状态，客户端需按 gRPC 语义判断结果。协议细节见 [gRPC over HTTP/2](https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md)。

### 4.2 长连接和多路复用意味着什么

一次一元 RPC 通常对应一个 HTTP/2 stream；多个 RPC 可以共享底层连接。这里的 HTTP/2 stream 是传输层概念，不等于业务接口声明为“流式 RPC”。一元方法同样使用它。

`ClientConn` 是客户端逻辑通道，会管理解析、连接状态和底层连接；它不等于一条永远固定的 TCP 连接。通常为目标服务创建后长期复用，可被多个 goroutine 并发调用，不应每次请求都创建和关闭。

本项目版本的 `grpc.NewClient()` 创建通道时不会立即进行网络 I/O；首次 RPC 会触发连接。因此返回 `err == nil` 不代表服务已经连通。应通过有超时的实际 RPC 或健康调用验证。参见 [grpc-go v1.70.0 NewClient 与 ClientConn](https://pkg.go.dev/google.golang.org/grpc@v1.70.0#NewClient)。

## 5. 逐段读懂项目中的 proto

源文件：`api/proto/catalog/v1/catalog.proto`。

```proto
syntax = "proto3";

package dingdong.catalog.v1;
option go_package = "github.com/Nobeta-Work/dingdong-go/api/gen/catalog/v1;catalogv1";

service Catalog {
  rpc GetSKU(GetSKURequest) returns (SKU);
}

message GetSKURequest { string sku_id = 1; }
message SKU {
  string sku_id = 1;
  string name = 2;
  int64 price_cent = 3;
  bool available = 4;
  int32 stock_available = 5;
}
```

### 5.1 三种“包名”别混淆

| 声明 | 作用 |
| --- | --- |
| `package dingdong.catalog.v1` | Protobuf 命名空间，组成完整 RPC 方法名 |
| `go_package` 分号前的路径 | 生成 Go 包的导入路径 |
| `go_package` 分号后的 `catalogv1` | 生成 Go 代码中的包名 |

所以 Go 导入可以写成：

```go
import catalogv1 "github.com/Nobeta-Work/dingdong-go/api/gen/catalog/v1"
```

`.proto` 不使用 Go 的 `package main` 语义；它是跨语言契约。Java 等语言可从同一份协议生成自己的消息类型和客户端，不需要共享 Go 源码。

### 5.2 service 是通信契约中的服务

`service Catalog` 定义一组 RPC 方法；它既不是 Go 进程，也不是一个现成的业务实现。本项目的商品服务进程已同时注册 `Catalog` 与 `Inventory` 两个协议服务。

`GetSKU` 的输入只能有一个消息类型、输出也有一个消息类型；多个参数放进请求 message，不能写成多个独立形参。这使后续添加字段更自然。

### 5.3 数据类型与实际业务

- `string sku_id`：边界协议使用字符串 ID；数据库内部可以仍使用 BIGINT，需要在服务边界转换。
- `int64 price_cent`：金额以分传递，避免把浮点数当作精确金额。
- `bool available`：当前真实实现映射 `sku.enabled`，表示商品启用，不代表实时库存充足。
- `int32 stock_available = 5`：M1 新增字段，来自 `inventory.available`，供发布时校验额度；它是查询时的库存事实，最终预占仍需原子更新。
- `enum`：项目中的预占状态、支付状态。0 值使用 `UNSPECIFIED`，便于识别未明确给出的状态。

### 5.4 零值不一定代表“用户传了这个值”

proto3 默认的普通标量字段采用隐式 presence：未设置和显式设置零值，解码后通常无法区分。例如 `bool enabled` 的 false 可能表示“明确禁用”，也可能表示“没传”。

对于局部更新请求，应按需要使用 `optional`：

```proto
// 说明性示例，不是当前项目已经存在的协议。
message UpdateSKURequest {
  string sku_id = 1;
  optional bool enabled = 2;
}
```

本项目使用的 proto3 Open Struct Go API 中，optional bool 通常表现为 `*bool`；nil 表示未设置，非 nil 且值为 false 表示明确传入 false。只使用 getter 取值会丢掉这种区分，需要先判断 presence。嵌套 message、`oneof` 等也有各自的 presence 语义。详见 [字段 presence](https://protobuf.dev/programming-guides/field_presence/)。

### 5.5 集合、互斥字段与兼容演进

需要列表时使用 `repeated SKU items`，键值表使用 `map<string, string>`；需要表达多个候选字段中只选择一个时使用 `oneof`。这些语法只定义数据结构，不会自动做业务校验，例如 quantity 大于 0 仍需实现检查。

演进协议时，新增字段通常比修改旧字段安全；新旧两端按各自协议处理字段。必须保持已有字段编号含义，不复用已删除编号，最好同时保留删除名称：

```proto
// 示意：假设某消息原有编号 5、字段名 old_name，现已删除。
message Example {
  reserved 5;
  reserved "old_name";
  string name = 1;
}
```

不要把旧字段从 string 随意改成 int64，也不要以为重命名完全无影响：二进制主要依赖编号，但 ProtoJSON、生成代码和调用方源码可能依赖名称。`v1` 只是命名空间约定，不会自动替你管理版本兼容。参见 [proto3 语言指南](https://protobuf.dev/programming-guides/proto3/)。

## 6. 生成代码到底生成了什么

### 6.1 三个工具分工

| 工具 | 工作 |
| --- | --- |
| `protoc` | 解析 `.proto`，调用生成插件 |
| `protoc-gen-go` | 生成 Go 消息及 Protobuf 运行时描述信息 |
| `protoc-gen-go-grpc` | 生成 gRPC 客户端、服务端接口和注册适配代码 |

仓库已提供固定版本脚本，正常使用时直接调用，不必手工重复安装步骤：

```bash
bash scripts/generate.sh
# 指定已安装 protoc 29.3 的实际路径：
bash scripts/generate.sh --protoc /path/to/protoc
```

脚本先检查 libprotoc 29.3，再将两个固定版本插件安装到仓库 bin，并执行消息和 gRPC 生成。Git Bash 的 GOBIN 路径转换、Windows 输出的回车处理已在脚本中实现，优先复用脚本；生成结果仍需随协议提交。已有 api/gen 可直接构建，不要求每次运行生成器。

脚本中的几个参数分别表示：

```text
-I api/proto                 协议 import 的查找根目录
--go_out=.                  输出 Go 消息代码
--go-grpc_out=.             输出 gRPC 代码
--go_opt=module=...         从 go_package 中去掉模块前缀，定位模块内输出目录
--go-grpc_opt=module=...    对 gRPC 输出使用相同规则
```

生成后有 `catalog.pb.go` 和 `catalog_grpc.pb.go`。需要提交生成结果，使普通使用者不必先安装生成工具；修改协议后重新生成，不手改生成文件。

两类文件的分工可以直接对应到 proto：

| 生成文件 | 主要对应 | 作用 |
| --- | --- | --- |
| `catalog.pb.go` | message、enum | SKU 等 Go 类型、getter、描述信息与 Protobuf 编解码运行时支持；可以脱离 gRPC 单独使用 |
| `catalog_grpc.pb.go` | service、rpc | CatalogClient、CatalogServer、默认实现、注册函数和方法分派；负责把类型连接到远程调用 |

例如 M1 给 SKU 增加 stock_available=5，会使消息类型出现 StockAvailable/GetStockAvailable；新增或修改 RPC 方法则需要同步更新客户端/服务端接口。业务 SQL、库存事务和支付规则不会由任一生成器自动产生。

### 6.2 客户端：一个有类型的远程调用代理

在 `api/gen/catalog/v1/catalog_grpc.pb.go` 可以看到以下结构，省略了生成器的部分辅助代码：

```go
type CatalogClient interface {
    GetSKU(ctx context.Context, in *GetSKURequest, opts ...grpc.CallOption) (*SKU, error)
}

// 实际生成代码还会合并 grpc.StaticMethod() 等调用选项。
func (c *catalogClient) GetSKU(ctx context.Context, in *GetSKURequest, opts ...grpc.CallOption) (*SKU, error) {
    out := new(SKU)
    err := c.cc.Invoke(ctx, Catalog_GetSKU_FullMethodName, in, out, opts...)
    if err != nil {
        return nil, err
    }
    return out, nil
}
```

`NewCatalogClient(conn)` 只把连接通道包装成 Catalog 客户端，不会另开一个商品服务进程。`Invoke` 把方法名和消息交给 gRPC 运行时；它返回时，`out` 已经被解码填充。

### 6.3 服务端：接口、默认实现、注册表

生成的 `CatalogServer` 要求实现 `GetSKU`，并通过嵌入 `UnimplementedCatalogServer` 保留前向兼容能力：

```go
type catalogServer struct {
    catalogv1.UnimplementedCatalogServer
}
```

这里是 Go 的匿名字段嵌入，字段名就是类型名。它能提升被嵌入类型的方法；自己定义的 `GetSKU` 会提供真正处理逻辑。默认实现遇到未覆盖的方法返回 `codes.Unimplemented`。

应按值嵌入，而非嵌入未初始化的 `*UnimplementedCatalogServer`；生成器也会检查相应条件，以免 nil 指针导致运行时错误。

服务端需要显式注册：

```go
rpcServer := grpc.NewServer()
catalogv1.RegisterCatalogServer(rpcServer, &catalogServer{})
```

注册函数将服务描述与实现对象关联。收到请求后，生成的 `_Catalog_GetSKU_Handler` 解码 `GetSKURequest`、经过服务端 interceptor，再调用你实现的方法。

**必须把服务注册到实际执行 `Serve` 的那个 `grpc.Server` 上，并在 `Serve` 之前注册。** 只创建 handler、只生成代码、或者注册在另一个 server 上都无法对外提供该方法。Go 端基本开发流程见 [官方 Go 教程](https://grpc.io/docs/languages/go/basics/)。

## 7. 当前 M1/M2 的真实 RPC 实现

| 环节 | 当前实现位置与能力 |
| --- | --- |
| 商品协议 | api/proto/catalog/v1/catalog.proto：Catalog、Inventory，SKU 新增 stock_available=5 |
| 支付协议 | api/proto/payment/v1/payment.proto：创建/获取、查询、关闭 |
| 商品服务端 | catalog/http.go 的 catalogService，实现 GetSKU 并注册 Catalog |
| 库存服务端 | catalog/inventory.go 的 inventoryService，实现四个库存方法 |
| 支付服务端 | payment/payment.go 的 service，实现三个 Payment 方法 |
| 注册入口 | 各服务 http.go 的 configure，使用 Runtime.RPC |
| 交易客户端 | trade/models.go 的 newService，构造三个生成客户端 |
| 远程业务调用 | trade/activity.go、order.go 中实际调用商品、库存和支付 |
| 公共生命周期 | platform/app/app.go 先 Configure 后 Serve，管理通道与监听退出 |

标准 Health 与业务 RPC 是分别注册的服务，但共用各自的 gRPC server。当前不能再把业务调用失败笼统解释为“仅框架、未实现”：应检查实际方法、地址、参数和依赖状态。

### 7.1 装配如何连接公共框架与业务

`Spec.Configure` 的类型是 `func(*Runtime) (func(context.Context) error, error)`。app.Run 创建 DB、Router、RPC 和客户端连接后调用它。业务在回调里注册 handler，并返回后台资源清理函数。

商品服务当前实际注册代码：

```go
s := &catalogService{db: r.DB}
pb.RegisterCatalogServer(r.RPC, s)
pb.RegisterInventoryServer(r.RPC, &inventoryService{db: r.DB})
```

支付服务调用 `pb.RegisterPaymentServer(r.RPC, s)`。注册后，app 在同一个 r.RPC 上安装标准健康服务并执行 Serve。这样业务包可以取得资源，但公共平台不必导入商品或支付包。

### 7.2 一个目标通道上可以有多个协议客户端

交易服务的 newService 使用 r.CatalogConn 创建 CatalogClient 和 InventoryClient，使用 r.PaymentConn 创建 PaymentClient。HealthClient 也可以复用这些通道；不同代理只决定调用哪个完整服务/方法名，不意味着要各开一个业务进程。

连接由 app 生命周期管理，业务长期复用。真实调用链继续见第 9 节；完整状态机见 [M1 业务流程](M1业务流程.md)。

## 8. 动手：跑通第一个商品查询 RPC

这个练习复用现有协议和依赖，只增加两个独立学习程序，不需要启动现有四服务、数据库或 Docker。服务端使用固定的演示商品验证通信，因此它是**教学数据源，不是当前商城业务的实现或验收**。

本节代码给出完整文件，可直接复制。所有命令在 `dingdong-go` 根目录运行。建议放在 `docs/explain/grpc-demo/`，既便于保留学习材料，也不会把演示接口混进正式服务。当前 `docs/` 被 Git 忽略，这些练习默认只留在本地。

### 8.1 创建练习目录

```bash
mkdir -p docs/explain/grpc-demo/server docs/explain/grpc-demo/client
```

目录中不要新增 `go.mod`：它们作为当前 module 内的程序，直接使用仓库依赖和 `api/gen`。

### 8.2 服务端完整文件

保存到 `docs/explain/grpc-demo/server/main.go`：

```go
// Command server 演示如何把已有 Catalog 协议注册为可调用的 gRPC 服务。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	catalogv1 "github.com/Nobeta-Work/dingdong-go/api/gen/catalog/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// catalogServer 实现生成的接口；按值嵌入默认实现以保持前向兼容。
type catalogServer struct {
	catalogv1.UnimplementedCatalogServer
}

// 编译期断言：若方法签名不符合协议，此处会编译失败。
var _ catalogv1.CatalogServer = (*catalogServer)(nil)

// GetSKU 只查询一个教学商品，用来观察成功、参数错误和不存在三种结果。
func (s *catalogServer) GetSKU(ctx context.Context, req *catalogv1.GetSKURequest) (*catalogv1.SKU, error) {
	if req.GetSkuId() == "" {
		return nil, status.Error(codes.InvalidArgument, "sku_id 不能为空")
	}
	if req.GetSkuId() != "1" {
		return nil, status.Error(codes.NotFound, "商品不存在")
	}
	// 真实业务应在这里调用业务层；此处不写数据库，也不处理库存。
	return &catalogv1.SKU{
		SkuId:     "1",
		Name:      "叮咚演示商品1",
		PriceCent: 1990,
		Available: true,
	}, nil
}

func run() error {
	// 选用独立学习端口，避免与 Compose 的 19000 端口冲突。
	listener, err := net.Listen("tcp", "127.0.0.1:29000")
	if err != nil {
		return err
	}
	defer listener.Close()

	server := grpc.NewServer()
	catalogv1.RegisterCatalogServer(server, &catalogServer{})

	// Serve 会持续阻塞，因此放入 goroutine，主流程同时等待退出信号。
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()
	slog.Info("grpc_demo_started", "addr", listener.Addr().String())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-serveErrors:
		if errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	// GracefulStop 等待在途 RPC 完成；超时后 Stop，避免退出无限等待。
	stopped := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(stopped)
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-stopped:
	case <-timer.C:
		server.Stop()
		<-stopped
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("grpc_demo_failed", "error", err)
		os.Exit(1)
	}
}
```

关键位置解释：

1. `net.Listen` 负责占用地址并等待 TCP 连接，它本身不认识 Catalog 方法。
2. `grpc.NewServer` 创建 RPC 运行时，它本身也没有商品业务。
3. `RegisterCatalogServer` 把协议服务名与 `catalogServer` 实现关联起来。
4. `Serve(listener)` 使服务器真正开始接受请求并分派方法。
5. 方法返回 `(*SKU, nil)` 表示成功，返回 `(nil, status.Error(...))` 表示明确的 RPC 失败。

`(*catalogServer)(nil)` 是“类型为该结构体指针的 nil 值”，编译期断言仅检查接口实现关系，不会创建一个要使用的业务实例。

Handler 可能并发执行。示例每次创建新响应，没有共享可变业务状态；以后不能直接加一个普通 map 并在多个 RPC 中并发修改而不加保护。

### 8.3 客户端完整文件

保存到 `docs/explain/grpc-demo/client/main.go`：

```go
// Command client 演示复用 gRPC 通道、设置调用超时和识别状态码。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	catalogv1 "github.com/Nobeta-Work/dingdong-go/api/gen/catalog/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func run() error {
	conn, err := grpc.NewClient(
		"dns:///127.0.0.1:29000",
		// ! 仅用于本机练习的明文连接；它不提供 TLS 加密与服务端身份校验。
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := catalogv1.NewCatalogClient(conn)

	skuID := "1"
	if len(os.Args) > 1 {
		skuID = os.Args[1]
	}
	// 每次 RPC 都有自己的预算；NewClient 的成功不代表远端可达。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sku, err := client.GetSKU(ctx, &catalogv1.GetSKURequest{SkuId: skuID})
	if err != nil {
		return fmt.Errorf("GetSKU 失败，code=%s: %w", status.Code(err), err)
	}
	fmt.Printf("skuId=%s name=%s priceCent=%d available=%t\n",
		sku.GetSkuId(), sku.GetName(), sku.GetPriceCent(), sku.GetAvailable())
	return nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("grpc_demo_call_failed", "error", err)
		os.Exit(1)
	}
}
```

这里两个创建函数的作用不同：`grpc.NewClient` 管理目标地址与通信通道，`NewCatalogClient` 提供协议对应的有类型方法。相同连接还可以构造 `NewInventoryClient`，但练习服务端没有注册 Inventory，所以调用库存方法仍然会返回 Unimplemented。

### 8.4 启动并观察

先在终端 A 执行：

```bash
go run ./docs/explain/grpc-demo/server
```

看到 `grpc_demo_started` 后，在终端 B 执行：

```bash
go run ./docs/explain/grpc-demo/client 1
```

预期输出：

```text
skuId=1 name=叮咚演示商品1 priceCent=1990 available=true
```

再查询不存在的商品：

```bash
go run ./docs/explain/grpc-demo/client 999
```

此时会记录 `code=NotFound` 并以非零退出，`go run` 还可能输出 `exit status 1`。这是远程业务拒绝被客户端正确识别，不是通信代码失效。

想观察 `InvalidArgument`，可以暂时把客户端请求改为 `SkuId: ""`，然后重新运行客户端。练习结束后在终端 A 按 Ctrl+C 停止服务。

如果先启动客户端而服务端还没监听，通常会得到 `Unavailable`，也可能因连接/调用阶段超时得到 `DeadlineExceeded`，不要把所有失败都理解成“超时”。

## 9. 对照真实源码：从 HTTP 走到 RPC 与数据库

第 8 节固定商品只用于独立练习。本节讲解当前业务的真实实现，无须再按旧版教程新建 catalog/rpc.go，也不需要再改造框架以暴露资源：Configure/Runtime 已经实现。

### 9.1 GetSKU 的实现在哪，怎样读取真实数据

文件：internal/catalog/http.go。catalogService 按值嵌入生成的 UnimplementedCatalogServer，并持有所属库连接池。

当前 GetSKU 的主要查询为：

```sql
SELECT s.id,s.name,s.price_cent,s.enabled,i.available
FROM sku s JOIN inventory i ON i.sku_id=s.id
WHERE s.id=?
```

使用 QueryRowContext(ctx, ..., in.SkuId) 和 Scan，分别写入生成消息的 SkuId、Name、PriceCent、Available、StockAvailable。占位符传参不拼接业务值，ctx 从本次调用继续传给数据库。

available 表示商品启用，stock_available 表示当前可用件数。查不到 SKU 或其库存关联行时返回 NotFound，数据库未知错误经 rpcError/公共 interceptor 转换。这里没有预占，其他请求仍可能改变库存。

商品 HTTP 查询也直接调用这个 GetSKU 实例方法，然后只输出公开字段，不输出 stock_available。它是同进程复用，不是在 HTTP handler 里重新发起一次网络请求。

### 9.2 trade 怎样调用 Catalog.GetSKU

trade/models.go 中的 service 保存生成接口字段，newService 从 Runtime 的长期通道构造客户端。trade/activity.go 中 createActivity 和 publish 都实际调用 s.catalog.GetSKU。

真实调用模式如下：

```go
rpcCtx, cancel := rpcContext(ctx)
sku, err := s.catalog.GetSKU(rpcCtx, &catalogpb.GetSKURequest{SkuId: a.SKUID})
cancel()
if err != nil {
    return activity{}, err
}
if !sku.Available || a.Quota > int(sku.StockAvailable) {
    return activity{}, status.Error(codes.FailedPrecondition, "发布额度超过当前可用库存或商品未启用")
}
```

rpcContext 在当前源码中返回 context.WithTimeout(parent, time.Second)。每个调用单独派生并及时 cancel，保留上游取消语义。

创建草稿时查询商品名称并保存快照，活动价格来自运营提交的 priceCent；发布时再次检查启用状态与额度。发布校验不是库存锁定，最终能否成单仍由 Inventory.Reserve 的原子更新决定。

### 9.3 Inventory.Reserve 为什么不只是执行一次减法

实现：catalog/inventory.go 的 change。请求数量固定为 1，以 order_no+sku_id 为幂等键，在同一事务中维护预占流水和库存。

```text
BeginTx
  → INSERT 预占流水，遇到同键不覆盖
  → SELECT 流水 FOR UPDATE
  → 检查状态与数量是否一致
  → 仅首次预占：available-1、locked+1，要求 available>=1 且 SKU 启用
  → RowsAffected 不是 1：返回 ResourceExhausted，事务回滚
Commit
```

同键已为 LOCKED/CONFIRMED 时不重复扣减；已为 RELEASED 时拒绝迟到的 Reserve。app 显式禁用 MySQL ClientFoundRows，使 RowsAffected 能区分新插入与无变化重放，这是当前幂等实现依赖的驱动细节。

Confirm 在同一事务中锁流水，将 locked 转 sold；Release 将 locked 转回 available。如果 Release 先于 Reserve 到达，会先写 RELEASED 标记，使迟到写入无法重新占用库存。

### 9.4 超时回查已经落实在哪里

trade/order.go 的 createOrder 首先调用 Reserve。ResourceExhausted/FailedPrecondition 是明确拒绝；其他不确定错误会调用 GetReservation：LOCKED/CONFIRMED 视为已有事实，RELEASED 视为终止，查询也失败则保留 PROCESSING 并返回错误等待恢复。

createPayment 调用 CreateOrGet 发生不确定错误时调用 QueryByOrder，再核对用户、金额和截止时间。settleInventory 的 Confirm/Release 失败也会回查预占，只有观察到目标状态，才将交易库的 inventory_status 更新为相应终态。

这些查询方法解决的是“远端可能已提交但本端没收到结果”，不是为了省掉错误检查。代码没有通过一次超时就猜测缺货、已关闭或已释放。

### 9.5 Payment 服务端如何实现同键与终态

payment/payment.go 的 CreateOrGet 以订单号唯一插入，锁定读取后核对用户、金额、过期时间。重复请求返回原支付单；遇到 CLOSED 终止标记不能覆盖为 PENDING。

CloseByOrder 在不存在支付单时写 CLOSED 标记，已有 SUCCESS 则直接返回 SUCCESS；到期 PENDING 才转 CLOSED。simulate 通过本地事务锁定支付单，校验归属和期限，将成功事实与 PaymentSucceeded Outbox 同时提交。

因此 RPC CreateOrGet 不接受客户端自由指定订单金额：交易 HTTP 入口先读自己的订单，再把可信金额和 expiresAt 传给支付服务。

### 9.6 公共 interceptor 与当前限制

platform/app/app.go 已安装一元错误转换 interceptor：调用 handler；失败时记录完整方法名与 error；已有 status 原样保留；未转换的 sql.ErrNoRows、deadline、cancel 分别映射，其余底层错误映射 Unavailable。

具体业务也可能预先转换错误，例如 catalog.rpcError。最终状态取决于错误经过哪条转换路径，不应只看某一层。

当前已有 RPC 调用次数/耗时/失败/超时指标，客户端 interceptor 会注入 x-trace-id，服务端在失败日志读取。还没有通用成功调用逐条日志、RPC panic 恢复、TLS 或内部认证。Gin 的恢复中间件仍不会覆盖 gRPC。Health 的 SERVING 只表示启动状态，所属依赖应结合 HTTP /readyz 检查。

### 9.7 独立练习与真实应用的地址

独立练习使用 127.0.0.1:29000，只注册 Catalog。真实 catalog 宿主机端口 19000、容器内 9000，同时注册 Catalog 和 Inventory；payment 使用宿主机 19001。

真实服务运行、迁移和种子就绪后，可以用 grpcurl 只读调用查询，避免意外变更库存：

```bash
grpcurl -plaintext \
  -import-path api/proto \
  -proto catalog/v1/catalog.proto \
  -d '{"sku_id":"1"}' \
  localhost:19000 dingdong.catalog.v1.Catalog/GetSKU
```

真实 RPC 依赖数据库和配置；练习程序不依赖它们。不要把练习里固定的 1990 分商品当作真实数据库查询的验证结果。

## 10. 超时、取消、错误与重试：实际使用的关键

### 10.1 Deadline 是一次调用的时间预算

客户端通过 `context.WithTimeout` 设定预算。gRPC 默认不替应用设置业务调用的 deadline；实际调用应明确设置。

真实 HTTP → RPC 链路应从 HTTP 请求上下文派生：

```go
// Gin Handler 内的调用片段，client 为装配时注入的 CatalogClient。
ctx, cancel := context.WithTimeout(c.Request.Context(), 800*time.Millisecond)
defer cancel()
sku, err := client.GetSKU(ctx, &catalogv1.GetSKURequest{SkuId: skuID})
```

如果上游请求只剩 300 毫秒，派生出来的 ctx 不会因为写了 800 毫秒而获得更长寿命。真实请求链路中不要改用 `context.Background()`，否则会断开上游的取消与预算传递。

服务端应该把收到的 ctx 继续传给数据库或下游 RPC。取消是协作机制，不会杀死任意 Go 代码，也不会回滚已经提交的事务；长时间循环或工作需要主动观察取消。参见 [gRPC Deadline 指南](https://grpc.io/docs/guides/deadlines/)。

示例中的 2 秒、800 毫秒只是学习值。正式预算应根据总请求时限、下游耗时和重试成本确定。

### 10.2 gRPC 错误不是 HTTP 响应体

当前 `httpx.Response` 的 `code/message/data/traceId` 是 HTTP 对外协议，不应直接套到每个 RPC 响应中。gRPC 通常使用响应消息表示成功，用 `status` 表示失败。

| gRPC code | 典型含义 | 本项目可对应的情况 |
| --- | --- | --- |
| `InvalidArgument` | 请求参数不符合要求 | SKU ID 格式错误、数量不合法 |
| `NotFound` | 目标资源不存在 | 查询不到 SKU 或预占记录 |
| `AlreadyExists` | 资源已经存在 | 某创建契约明确拒绝重复创建；幂等创建也可以直接返回旧结果 |
| `FailedPrecondition` | 当前状态不允许操作 | 对已关闭支付单执行不允许的操作 |
| `ResourceExhausted` | 资源或额度不足 | Reserve 表达 SKU_STOCK_UNAVAILABLE；M2 HTTP 限流也用该状态并转为 429，两条调用路径需区分 |
| `Unauthenticated` / `PermissionDenied` | 身份无效 / 权限不足 | 内部调用方未认证或无权限 |
| `Unavailable` | 服务暂不可用 | 无法建立连接、目标暂时离线 |
| `DeadlineExceeded` | 调用预算耗尽 | 等待超过截止时间 |
| `Canceled` | 调用已取消 | 上游取消请求 |
| `Unimplemented` | 服务或方法未实现 | 连错服务、未注册方法；独立练习未注册 Inventory |
| `Internal` | 内部实现错误 | 不适合直接暴露的 SQL/系统错误 |

表中结合了状态码语义与当前业务映射，具体还需对照 handler 和公共 interceptor。客户端根据 `status.Code(err)` 分支处理，不解析错误文字。一般 `fmt.Errorf(...)` 未转换时，原生 gRPC 通常将其作为 Unknown；本项目的公共 interceptor 会先转换未识别底层错误，通常返回 Unavailable，不能直接套用“普通错误一定是 Unknown”。HTTP 映射由 httpx.Error 另行完成。状态码语义见 [gRPC 状态码](https://grpc.io/docs/guides/status-codes/)。

如果一种“拒绝”是调用方频繁需要消费的正常业务结果，也可以在协议中定义明确结果枚举，但要统一规则，避免相同条件有时返回成功消息、有时返回 status 错误。

### 10.3 超时不代表写入失败

假设 `Inventory.Reserve` 发生以下过程：

```text
trade 发起预占 → catalog 提交库存事务 → 响应传输中断 → trade 超时
```

此时库存可能已经锁定。trade 不能据此认定“没有预占”，也不能换一个新业务编号再次扣库存。

正确设计需要依赖稳定的业务键，例如项目协议中的 `(order_no, sku_id)`：重复请求先查询已有结果，核对参数，符合约定则返回同一结果。唯一索引是基础，还需要事务、状态判断以及失败后的查询或补偿。

### 10.4 重试必须服从业务幂等

查询通常较容易安全重试；预占、支付、确认等写操作需要先定义幂等语义。应明确哪些错误可以重试、最多次数、退避间隔和总 deadline，避免无上限放大流量。

gRPC 存在透明重试和按服务配置启用的策略重试；未配置业务重试策略，不等于底层绝不会重复某个调用阶段。因此即便调用方“不主动重试”，写接口也应该考虑重复请求。

项目当前没有给业务写调用配置全局重试，不能把这理解为已经具备业务可靠性保证。详见 [gRPC 重试机制](https://grpc.io/docs/guides/retry/)。

`WaitForReady` 只影响暂时不可用时是否在 deadline 内等待可用通道，不会把请求持久化，也不提供 MQ 那种离线保留与消费语义。

## 11. Metadata、Interceptor 与连接安全

### 11.1 Metadata 传调用上下文，message 传业务数据

Metadata 是随 RPC 传递的键值信息，适合调用凭据、追踪标识等；商品 ID、数量等业务参数仍应在 message 中定义。

客户端片段：

```go
// 需要导入 google.golang.org/grpc/metadata。
// ctx 应已经带有本次调用的 deadline。
ctx = metadata.AppendToOutgoingContext(ctx, "x-trace-id", traceID)
sku, err := client.GetSKU(ctx, &catalogv1.GetSKURequest{SkuId: skuID})
```

服务端片段：

```go
md, ok := metadata.FromIncomingContext(ctx)
if ok {
    values := md.Get("x-trace-id")
    if len(values) > 0 {
        traceID := values[0]
        // 根据系统约定校验长度与格式后，用于日志关联。
        _ = traceID
    }
}
```

Metadata 的 incoming 与 outgoing 是不同方向；收到后继续调用下游时，需要明确选取并转发，不应无条件复制所有字段。键通常使用小写，二进制字段使用 `-bin` 后缀，`grpc-` 前缀保留给框架。详见 [Metadata 指南](https://grpc.io/docs/guides/metadata/)。

M2 已实际接入：HTTP 生成 traceId 后调用 httpx.WithTrace 放入 Go context；客户端 interceptor 用 httpx.TraceID 读取并注入 x-trace-id；服务端失败日志读取 incoming metadata。任务 handleTask 也把持久化 trace_id 放回 ctx，让库存/支付 RPC 延续关联。它提供基本跨调用关联，还不是完整 span 模型、采样或 tracing 后端；服务端也没有自动把任意 incoming 字段透传给所有下游。

### 11.2 Interceptor 类似 RPC 中间件

HTTP 中间件包裹 HTTP Handler，gRPC interceptor 包裹 RPC 处理过程，适合统一日志、认证、追踪和指标。一元与流式分别有对应的 interceptor；Gin 的中间件不会自动作用于 gRPC。

以下是一元服务端日志拦截器函数，可放到服务端文件中，需要增加 `google.golang.org/grpc/status` 导入（练习服务端已有该导入）：

```go
// unaryLog 为一元 RPC 记录方法名、耗时和最终状态，不记录完整敏感消息。
func unaryLog(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
    start := time.Now()
    // * handler 才会继续执行实际业务；忘记调用就会截断调用链。
    resp, err := handler(ctx, req)
    slog.InfoContext(ctx, "grpc_request",
        "method", info.FullMethod,
        "code", status.Code(err).String(),
        "durationMs", time.Since(start).Milliseconds(),
    )
    return resp, err
}
```

将练习中的 server 创建语句替换为：

```go
server := grpc.NewServer(grpc.ChainUnaryInterceptor(unaryLog))
```

`info.FullMethod` 就是 `/dingdong.catalog.v1.Catalog/GetSKU`。多个 interceptor 按传入顺序嵌套，进入时从前向后，返回时从后向前；调用前后都可以执行逻辑。

当前项目已在 `platform/app/app.go` 的 `grpc.NewServer(grpc.UnaryInterceptor(...))` 中接入调用指标、失败日志与错误映射；客户端也有追踪号与耗时/失败/超时 interceptor。上面的 unaryLog 是额外教学示例，不是对现有代码的逐字摘录；若要合并，需要保留已有错误转换并明确拦截器顺序。当前未实现 RPC 认证或 panic 恢复，Gin 中已有的恢复不会覆盖 gRPC。机制详见 [Interceptor 指南](https://grpc.io/docs/guides/interceptors/)。

### 11.3 明文、TLS 和业务鉴权是三件事

`insecure.NewCredentials()` 明确选择明文传输，并不是“跳过证书验证的 TLS”。练习和当前项目采用此方式，不提供加密。

实际 TLS 配置需要在服务端提供证书，在客户端配置可信 CA 与期望的服务身份；双向 TLS 还会验证客户端证书。TLS 保护传输与身份，具体业务权限仍需调用凭据和服务端校验。

因此“这是内网”“有 Health 服务”“metadata 中放了 token”都不能代替完整认证实现。本项目当前尚未实现内部 RPC 鉴权。参见 [gRPC 认证指南](https://grpc.io/docs/guides/auth/)。

## 12. RPC 在当前商城中的实际使用场景

### 12.1 草稿和发布：同步获取商品事实

交易服务 createActivity 查询 SKU、检查启用、保存商品名称与运营活动价格。publish 再查 SKU，验证 quota 不超过查询时的 stock_available，当前还需完成 admission 的重建与激活，再提供可用的已发布准入。

这适合 RPC：当前请求需要立即知道查询和校验结果。查询不会锁定未来库存，多活动竞争同一 SKU 时仍需最终预占判断。发布幂等不会重置 used_quota。

### 12.2 受理和成单：MQ 驱动，RPC 获取库存处理结果

M2 在 HTTP 重放和限流后 Lua 预扣，再由 persistTicket 将资格、额度、快照、稳定订单号、Outbox 与任务同事务提交，HTTP 返回 202。消息经 Inbox 转成可靠任务，handleTask 再调用 createOrder→Inventory.Reserve，成功后在交易库事务建单；本地兜底任务也可唤醒相同动作。

HTTP 请求不等待完整成单；客户端按 requestId 查询状态。RPC 对商品库的修改与交易库建单不处于同一事务，靠固定业务键、持久化中间态、重试和扫描恢复衔接。

### 12.3 支付：同步创建，异步推进订单与库存

trade 校验订单归属、状态与到期时间，RPC 创建/获取支付单；用户再通过 payment HTTP 模拟成功。payment 在同一事务中提交 SUCCESS 与事件，trade 消费后回查 QueryByOrder，核对事实再转 PAID。

PAID 与 InventoryConfirmRequested Outbox/兜底任务同事务提交，事件经 Inbox 转任务后调用 Confirm；只有交易库记录 PAID+CONFIRMED，fulfillable 才为 true。一次模拟支付 HTTP 成功不等于全链路立即完成。

### 12.4 到期关闭：必须以支付事实裁决

OrderExpiryDue 或扫描触发 closeOrder：先置 CLOSING，查询支付；不存在或 PENDING 则请求 CloseByOrder；返回 SUCCESS 就转已支付，只有明确 CLOSED 才取消并释放库存。网络错误保留 CLOSING，不能直接猜测未付款。

RPC 负责获得当前动作结果，MQ 与扫描负责持续推进。它们没有提供跨库原子提交，业务通过终态互斥和幂等收敛。

| RPC 契约 | 当前数据基础 | 解决的问题 |
| --- | --- | --- |
| Reserve / GetReservation | 预占唯一键与状态 | 重复预占、超时后的事实查询 |
| Confirm / Release | LOCKED、CONFIRMED、RELEASED | 库存计数与流水原子转换、拒绝迟到写入 |
| CreateOrGet | payment.order_no 唯一 | 重复创建同一支付单、参数一致性 |
| QueryByOrder | 持久化支付事实 | 判断已成功、未创建与技术未知 |
| CloseByOrder | CLOSED 终止标记 | 关闭先到时阻止迟到创建 |

完整事务顺序与失败路径见 [M1 业务流程](M1业务流程.md)。

## 13. 四种 RPC 模式

当前项目只定义一元 RPC，先把一元模式掌握即可。以下是语法示意，不要求修改现有协议：

```proto
service Example {
  rpc GetSKU(GetSKURequest) returns (SKU);                    // 一元
  rpc WatchSKU(GetSKURequest) returns (stream SKU);           // 服务端流
  rpc UploadSKU(stream SKU) returns (UploadResult);           // 客户端流
  rpc ExchangeSKU(stream SKU) returns (stream SKU);           // 双向流
}
```

其中 `UploadResult` 等类型在示意中未定义，因此这不是可以直接生成的完整协议。

| 模式 | 请求/响应 | 可能场景 |
| --- | --- | --- |
| 一元 | 一个请求，一个响应 | 商品查询、库存操作 |
| 服务端流 | 一个请求，多个响应 | 持续推送查询进度或订阅更新 |
| 客户端流 | 多个请求消息，一个最终响应 | 分批上传数据后汇总 |
| 双向流 | 两边都可持续发消息 | 交互式实时通信 |

流式 RPC 的 `Send/Recv` 操作也会受取消、deadline 和流控影响。收到正常 EOF 与收到状态错误要区分，客户端也需要正确关闭发送方向或结束接收。单个方向的消息顺序有保证，双向流两边独立推进，不代表所有消息都严格一问一答。

流式连接也不等于可靠 MQ：断线后的持久化、补发、断点和业务去重需要另行设计。概念与调用生命周期参见 [gRPC 核心概念](https://grpc.io/docs/what-is-grpc/core-concepts/)。

## 14. 调试和排障

### 14.1 先区分失败在哪一层

| 现象 | 常见原因 | 首先检查 |
| --- | --- | --- |
| `NewClient` 成功但 RPC 失败 | 通道创建尚未证明连接可达 | 实际错误码、监听地址、端口 |
| `Unavailable` | 服务没启动、端口错误、DNS 或连接故障 | server 启动日志；宿主机/容器地址区别 |
| `Unimplemented` | 业务服务未注册、方法名不一致、连错服务 | `.proto` 与注册函数、实际 Serve 的实例 |
| `DeadlineExceeded` | 预算耗尽、处理或连接阶段过慢 | ctx 预算、数据库耗时、下游链路 |
| `InvalidArgument` | 参数不满足约定 | 字段值与边界校验 |
| `Unknown` | 普通错误未转换为明确 status 等 | 服务端错误处理和日志 |
| TLS/握手相关错误 | 两端明文/TLS 不匹配、证书或主机名不匹配 | 客户端凭据与服务端 TLS 配置 |
| Health 返回 SERVING，但 GetSKU 不可用 | 健康协议与业务方法分开注册 | Catalog handler 是否实现并注册 |

不要只根据错误文字猜测；先看 gRPC code，再结合两端日志和方法名定位。

### 14.2 为什么不能拿普通 curl JSON 命令直接调用

`curl http://localhost:29000/GetSKU` 不是正确的 gRPC 请求：路径、HTTP/2 行为、消息 framing、编码和状态解析都不完整。普通 HTTP JSON 调试习惯不能原样套用。

可以使用上面的 Go 客户端，或者安装支持 gRPC 的工具。例如 grpcurl 已安装时，可在仓库根目录调用练习服务：

```bash
grpcurl -plaintext \
  -import-path api/proto \
  -proto catalog/v1/catalog.proto \
  -d '{"sku_id":"1"}' \
  127.0.0.1:29000 dingdong.catalog.v1.Catalog/GetSKU
```

`-plaintext` 与示例明文服务器对应；`-proto` 明确提供契约，因此不需要服务端反射。该外部工具并非项目自带依赖。用法参见 [grpcurl 项目说明](https://github.com/fullstorydev/grpcurl)。

反射服务能够让支持它的工具动态查询协议，但当前应用和练习都未注册反射。直接执行只依赖反射的列举命令失败，不等于业务 RPC 不通。

grpcurl 展示的是 JSON 表示，实际请求仍会编码成 Protobuf。例如 `int64` 在标准 ProtoJSON 中通常以 JSON 字符串表示；不要因此把协议字段改成 string。映射规则见 [ProtoJSON 格式](https://protobuf.dev/programming-guides/json/)。

### 14.3 健康检查和业务调用各看什么

标准 Health 是专门的 RPC 服务，不是自动执行每个业务方法的测试。本项目对它设置空服务名 `""` 的整体 SERVING 状态；这个状态当前没有动态汇总 MySQL/MQ 状态。

现有应用的依赖就绪状态应结合 HTTP `/readyz` 判断，而业务能力还要实际调用相应方法验证。运行中的 Health 机制和客户端联动需要明确配置，参见 [gRPC Health 指南](https://grpc.io/docs/guides/health-checking/)。

## 15. 上手顺序与自查

建议按以下顺序练习，不要同时改协议、数据库、业务和部署：

1. 直接使用已有生成代码，跑通第 8 节两个程序。
2. 查询 ID 1 与 999，对照 OK 和 NotFound；发送空 ID 观察 InvalidArgument。
3. 在服务端 GetSKU 中临时加入遵守 ctx 取消的等待，缩短客户端预算，观察 DeadlineExceeded；完成后移除教学等待。
4. 加入第 11 节日志 interceptor，观察完整方法名、耗时和 code。
5. 用 metadata 传递一个追踪号，在两端记录并核对。
6. 对照第 9 节真实 GetSKU、Configure/Runtime 和 newService，追踪现有应用的完整只读调用链；基础环境就绪后再手动调用真实查询。
7. 阅读已有库存写实现，标出幂等键、事务范围、状态转换和超时回查；再结合业务文档理解消费重投及恢复扫描如何复用它。

可以用这些问题检查自己是否真正理解：

- 能否指出 `.proto`、`.pb.go`、`_grpc.pb.go` 和自己写的 handler 分别做什么？
- 能否解释为什么 proto 字段编号不能复用，以及 `= 1` 为什么不是默认值？
- 能否说明 `NewClient`、`NewCatalogClient`、`RegisterCatalogServer`、`Serve` 的区别？
- 能否从项目生成代码找到 `Invoke`、完整方法名和服务端 Handler？
- 能否解释 Health 成功而业务返回 Unimplemented 的原因？
- 能否说明客户端超时后为什么不能直接断定库存未扣减？
- 能否在 HTTP → gRPC → SQL 链路中持续传递 ctx，并按 code 处理错误？

本文第 8 节练习只验证 gRPC 通信能力；第 9、12 节解释已实现业务，运行它们需要真实数据库、配置与依赖。学习程序不会替代 M1 交易闭环验收，也不会修改当前服务。

## 历史独立示例验证与本次更新范围

2026-10-24，上一版教程曾在本机 Go 1.27.1 windows/386 环境中提取完整学习示例进行验证：

- 独立服务端、客户端以及当时的教学 SQL 适配文件均编译通过；本版第 9 节已用真实 M1 源码讲解替换该 SQL 适配示例。
- 服务端监听本机回环地址 29000，真实客户端查询 ID 1，得到 `priceCent=1990 available=true`。
- 同一服务端收到 ID 999 查询，真实客户端得到 `NotFound` 并以非零状态退出。
- 临时验证服务已停止，验证文件已清理；两个练习程序仍以本文代码块提供，按第 8 节复制即可运行。

2026-10-25，本次按当前 M1 源码更新装配、真实 SQL/RPC、库存幂等、支付与关闭调用链，并核对文档路径和状态描述；未重新运行示例或交易联调。上一版独立通信验证不代表当前 M1 已完成运行验收。TLS、内部认证、完整 tracing 等仍为扩展知识，未实现的边界已在正文标明。`docs/` 沿用仓库忽略规则，本文件默认保存在本地。

## 16. M2 的 RPC 与持久化任务衔接

RPC 并没有被 Inbox/Queue 替代：队列只决定哪个业务键何时尝试，createOrder/closeOrder/settleInventory 仍同步调用生成客户端。每个 RPC 一秒预算，任务另有十秒处理预算、队列十五秒租约，三个时限是不同层次，不能互相代替。

任务 handler 返回错误时，Queue 决定退避与 NEEDS_REVIEW；它不会据此把订单改成 FAILED。远程写结果未知时仍先按业务键回查；已提交的 RPC 也不会因队列领取过期而撤销。因此库存/支付幂等和终态标记仍是业务正确性的基础。

阅读串联：[trade/worker.go](../../internal/trade/worker.go) 的 handleTask → [trade/order.go](../../internal/trade/order.go) → [app 客户端 interceptor](../../internal/platform/app/app.go) → 生成客户端 Invoke → [catalog/inventory.go](../../internal/catalog/inventory.go) 或 [payment/payment.go](../../internal/payment/payment.go)。

2026-10-26 本次更新核对了 M2 的 metadata 传播、调用指标与任务执行链，未重新运行示例或业务。前文历史独立 RPC 验证不能作为当前 M1/M2 验收结果。
