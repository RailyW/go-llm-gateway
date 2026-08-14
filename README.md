# go-llm-gateway

一个**最小可用**的 LLM 网关转发服务（对标 new-api 的极简版），Go + React。

只做核心闭环：**上游录入 → 模型录入 → 模型/上游绑定 → 归属化的上游 key → 发放自己的 API Key → 按端点同协议转发**。

## 上游 key 的选取链路

```
客户端请求 (model=网关模型名, 打某个端点)
  └─ 模型 ─► 绑定 (上游渠道 + 上游模型名)        # 只保留支持该端点协议的上游
       └─ 调用者所属「归属」(部门)
            └─ 该上游 × 该归属 下的可用 key 集合   # 一个渠道一个归属可以配多把
                 └─ key 选择策略 (random / weighted / affinity-hash)
```

- **归属 (Group)** 类似部门，在设置页维护枚举；每个用户属于一个归属（admin 在用户页可改）
- 上游 key 表是 `渠道 × 归属 × N 把 key`：不同归属用不同 key，天然做到**用户只能用自己归属下的 key**
- 某上游在调用者归属下**没有可用 key** 时，这条绑定不参与路由；全都没有则明确报错
- key 选择策略可插拔（`internal/relay/keyselector`），已内置 `affinity-hash`：同一网关 key 固定粘同一把上游 key（上游 prompt cache 友好）

## 转发模型：同协议直转，不做协议翻译

每个「协议端点」一一对应，客户端打哪个端点，就转到上游的同名端点：

| 客户端请求网关 | 转发到上游 | 协议名 | 鉴权头 |
| --- | --- | --- | --- |
| `POST /v1/chat/completions` | `{base_url}/v1/chat/completions` | `openai-chat` | `Authorization: Bearer <上游key>` |
| `POST /v1/responses` | `{base_url}/v1/responses` | `openai-responses` | `Authorization: Bearer <上游key>` |
| `POST /v1/messages` | `{base_url}/v1/messages` | `anthropic-messages` | `x-api-key` + `anthropic-version` |

请求体除了把 `model` 换成上游模型名之外**原样透传**，响应（含 SSE 流）**原样回吐**。
上游录入时勾选它支持哪些协议端点；路由时只有勾了对应协议的绑定才参与选择。

## 性能：热路径不落库

早期实现每个请求在 handler 里同步做 7 次 SELECT + 2 次文件写 + 3 次写事务（1 条日志 INSERT +
2 条 `last_used_at` UPDATE）。sqlite 是**全局单写者**且每次 commit 一次 fsync，于是：

- 吞吐被压在 ~220 RPS，且 **c=1 和 c=50 的 RPS 一样**（说明是全局串行，加并发没用）
- p99 2.5s、单条 INSERT 最慢 4.4s（`busy_timeout` 是 5s，再高一点就开始静默丢日志）
- 网关自己记的 `duration_ms` 却是 p50=0ms —— 时间全花在响应发出**之后**的落库上

现在的做法（同一台机器、同一个空转上游、非流式最小请求）：

| 场景 | RPS | p50 | p99 | max |
| --- | --- | --- | --- | --- |
| 直连上游（基线，不经网关） | 59,401 | 0.58ms | 4.6ms | 11ms |
| 改造前 c=50 | 219 | 41ms | 2.50s | 5.05s |
| **改造后 c=50** | **16,698** | **2.2ms** | **21.9ms** | 25.5ms |
| **改造后 c=200** | **20,263** | 8.5ms | 31.1ms | 52.1ms |
| 改造前 c=1 | 225 | 4.26ms | 6.9ms | 9.9ms |
| **改造后 c=1** | **4,270** | **0.18ms** | 1.9ms | 3.2ms |

吞吐 **76 倍**，p99 好 **114 倍**，单请求净开销 4.26ms → 0.18ms。四件事：

1. **`synchronous=NORMAL`**（WAL 下每次 commit 不再 fsync）。实测单条写入 1.575ms → 0.177ms（9 倍）。
   代价：机器掉电/内核崩溃可能丢最近几个事务，不会损坏数据库——对请求日志这类观测数据划算。
2. **日志与归档异步批量落库**（`internal/sink`）。请求只把 Entry 丢进有界队列；后台协程攒批，
   在**单个事务**里批量 INSERT，一批只有一次 fsync。实测批量 0.093ms/条 vs 逐条 1.575ms（17 倍）。
3. **`last_used_at` 合并**。这字段是 last-write-wins，天生可合并：每批只发 1 条
   `UPDATE ... WHERE id IN (...)`，而不是每请求 2 条。写事务从 3/请求降到 3/批。
4. **配置内存快照**（`internal/registry`）。网关 key、用户、归属、模型、绑定、上游 key
   全量缓存，用 `atomic.Pointer` 发布，热路径**零查询零锁**。任何管理 API 的写操作成功后
   由中间件同步重建快照（保证"改完立刻生效"），另有 30s 兜底刷新。

### 异步落库的代价（都是显式选择）

- **队列满会丢日志**（不阻塞转发）。实测落库上限约 **8k~13k 条/s**（瓶颈是 GORM 每行序列化，
  加大攒批帮助不大）；超过就开始丢。丢弃数、队列占用、上批耗时全部显示在概览页
  「异步落库管道」卡片里 —— **隐性丢数据是不可接受的，所以必须可见**。
- **日志有可见延迟**（默认 ≤200ms 或攒满 200 条）。攒批间隔/条数在设置页可调。
- **崩溃丢最后一批**。`SIGTERM` 优雅退出会先停 HTTP、再把队列刷完（实测入队 421 / 落库 421 / 丢弃 0）；
  `kill -9` 则丢最后一批。
- **写顺序**：每批先写归档文件、再插日志行，保证「日志里能看到的请求，原文一定已存在」。

### 流式响应不再攒内存

原先流式响应为了归档会把整个 SSE 攒在内存（每请求上限 8MB），20 个并发长回答就是 160MB。
现在是**边转发边追加写文件**（64KB bufio），实测 20 个并发 × 12MB 流式响应：
进程 RSS 仅 +19MB，归档文件完整 12.2MB 落盘。

概览页的统计只按**时间窗口**（1 小时 / 1 天）查询，不做全表 `COUNT/SUM`——
logs 表会一直增长，全表扫描的首页迟早会拖垮。更长周期的统计等以后做小时级 rollup。

## 功能范围

| 模块 | 说明 |
| --- | --- |
| 用户 | 注册 / 登录（JWT），角色 `admin` / `user`，归属（部门），首次启动自带 `admin / admin` |
| 归属 (Group) | 部门枚举，设置页维护；决定用户能用哪些上游 key；有用户/key 挂着或是默认归属时不可删 |
| 上游 (Channel) | `base_url` + **支持的协议端点多选**（openai-chat / openai-responses / anthropic-messages） |
| 上游 Key (ChannelKey) | 归属于「某上游 + 某归属」，同一组合可多把；带权重、启停、最近使用时间 |
| 模型 (Model) | 对外暴露的模型名（客户端请求里的 `model`） |
| 绑定 (Binding) | 模型 → 上游 + **上游真实模型名**，支持一个模型绑多个上游 |
| 路由策略 | 选上游绑定：`random`（默认）/ `weighted`；选上游 key：`random`（默认）/ `weighted` / `affinity-hash`。都可在设置页切换、都可扩展 |
| API Key | 网关自己发放的 `sk-...`，归属用户，可停用/删除 |
| 网关端点 | `/v1/chat/completions`、`/v1/responses`、`/v1/messages`（都含 **SSE 流式**）、`GET /v1/models` |
| 日志 | `logs` 表：用户+归属、端点/协议、模型、上游+实际用的上游 key、tokens、耗时、状态码、错误 |
| 原文归档 | 每次请求的**请求原文 + 响应全文**落本地文件，文件名 = 日志的 request id |
| 清理服务 | 后台 goroutine 按保留天数（默认 7 天，WebUI 可改）删除归档与历史日志 |

不做：计费/额度、渠道健康检查与重试、协议互转（openai ↔ anthropic 的 body 翻译）、embeddings/images 等其它端点。

## 快速开始

```bash
# 依赖：Go 1.22+ / Node 20+
make build      # 构建前端并打包成单个二进制 bin/gateway（前端已 embed）
./bin/gateway   # 访问 http://localhost:8080 ，用 admin / admin 登录
```

开发模式（前后端分离热更新）：

```bash
make dev-backend   # 终端 1: 后端 :8080
make dev-web       # 终端 2: 前端 :5173（已配置 /api、/v1 代理到 8080）
make mock          # 终端 3(可选): mock 上游 :9911，无需真实 key 即可联调
```

联调步骤：`设置` 页建归属 → `上游` 页新增渠道、展开后按归属「加 Key」→ `模型` 页新增、展开点「绑定」→
`用户` 页确认自己的归属 → `API Key` 页新建 key → 调用：

```bash
# OpenAI chat/completions
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-你的网关key" -H "Content-Type: application/json" \
  -d '{"model":"你录入的模型名","stream":true,
       "messages":[{"role":"user","content":"hello"}]}'

# OpenAI responses
curl http://localhost:8080/v1/responses \
  -H "Authorization: Bearer sk-你的网关key" -H "Content-Type: application/json" \
  -d '{"model":"你录入的模型名","input":"hello"}'

# Anthropic messages（原生协议；网关侧鉴权也用网关自己的 key）
curl http://localhost:8080/v1/messages \
  -H "x-api-key: sk-你的网关key" -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"你录入的模型名","max_tokens":64,
       "messages":[{"role":"user","content":"hello"}]}'
```

## 目录结构

```
backend/
  cmd/server/main.go              启动、优雅退出
  internal/config/                环境变量配置
  internal/store/                 GORM 模型 / sqlite / 设置 KV（含默认值与缓存）
  internal/auth/                  JWT、bcrypt、sk- key 生成
  internal/relay/
    protocol.go                   ★ 端点协议接口 + 注册表 + 共用小工具
    protocol_openai.go            openai-chat / openai-responses（Bearer）
    protocol_anthropic.go         anthropic-messages（x-api-key + anthropic-version）
    relay.go                      转发主流程（协议无关：选上游 → 按归属选 key → 改模型名 → 直转 → 落日志）
    selector/selector.go          ★ 上游绑定选择策略（random / weighted）
    keyselector/keyselector.go    ★ 上游 key 选择策略（random / weighted / affinity-hash）
                                  （relay 热路径只读 registry 快照 + 投递 sink，不碰数据库）
  internal/archive/               请求/响应原文归档（流式为增量写文件）+ 按天清理
  internal/sink/                  ★ 异步批量落库管道（接口化，可换 Redis Stream 实现）
  internal/registry/              ★ 配置内存快照，转发热路径零查询
  internal/cleaner/               后台清理服务
  internal/httpapi/               gin 路由、中间件、各资源 handler、静态前端挂载
  internal/web/                   //go:embed dist（前端产物）
web/                              Vite + React + TS + Tailwind v4 + shadcn 风格组件
scripts/mock_upstream.py          本地 mock 上游
```

## 数据与文件

- sqlite：`data/gateway.db`（纯 Go 驱动，无需 CGO）
- 归档原文：`data/archive/<YYYY-MM-DD>/<request-id>.request.json` 与 `<request-id>.response.txt`
  - `request.json` 含元信息（用户、key、模型、上游 URL）+ 客户端原始 body
  - `response.txt` 非流式为完整响应体，流式为完整 SSE 文本（边转发边追加写，单文件上限 32MB 后截断）
  - 文件由异步管道在后台写入（每批先写文件、再插日志行）
  - 日志页点「原文」即读取这两个文件；被清理后提示已删除

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `GATEWAY_PORT` | `8080` | 监听端口 |
| `GATEWAY_DATA_DIR` | `./data` | 数据目录 |
| `GATEWAY_DB_PATH` | `<data>/gateway.db` | sqlite 路径 |
| `GATEWAY_ARCHIVE_DIR` | `<data>/archive` | 归档目录 |
| `GATEWAY_JWT_SECRET` | `dev-insecure-...` | **生产务必修改** |
| `GATEWAY_ADMIN_USER/PASS` | `admin` / `admin` | 初始管理员（仅无 admin 时创建） |
| `GATEWAY_ALLOW_REGISTER` | `true` | 是否允许自助注册（也可在设置页改） |
| `GATEWAY_LOG_QUEUE_SIZE` | `8192` | 异步落库队列容量（满了丢日志，不阻塞转发） |

WebUI 可改的运行时配置：归档保留天数、日志保留天数、清理间隔、上游绑定路由策略、**上游 key 选择策略**、
**新用户默认归属**、上游超时、注册开关、**异步落库攒批间隔/条数**。

老库升级说明：启动时若发现旧的 `channels.api_key` 列，会自动把它迁成 `default` 归属下的一把 `ChannelKey` 并删除该列。

## 扩展点

**加一个协议端点**（如 gemini `/v1beta/models/*:generateContent`、或 anthropic 的 `/v1/messages/count_tokens`）：
在 `internal/relay/` 新建一个文件实现 `Protocol` 并在 `init()` 里 `Register`。
网关路由、上游的协议多选框、日志里的端点列都会自动出现，`relay.go` 主流程不用动。

```go
type Protocol interface {
    Name() string            // 协议键，如 anthropic-messages
    Label() string           // WebUI 展示名
    Vendor() string          // UI 分组：openai / anthropic / ...
    InboundPath() string     // 网关暴露路径
    UpstreamPath() string    // 上游路径

    BuildRequest(ctx, ch *store.Channel, req *ProtoRequest) (*http.Request, error) // URL + 鉴权头
    ParseRequest(body []byte) (model string, stream bool, err error)               // 读 model / stream
    ReplaceModel(body []byte, upstreamModel string) ([]byte, error)                // 只换模型名
    MergeUsage(payload []byte, acc *Usage)                                         // usage 字段位置/命名
    ErrorBody(status int, msg, requestID string) []byte                            // 网关自身错误格式
}
```

三个已实现协议的差异就只有：**路径、鉴权头、usage 字段、错误体格式**——body 不碰。

**加一个上游选择策略**（如轮询、最少失败）：在 `internal/relay/selector/` 实现 `Selector` 并 `Register`。

**把落库换成 Redis / ClickHouse**：`sink.Sink` 是接口（`Submit` / `Stats` / `Close`），
换一个 Redis Stream 实现即可，请求侧代码不动。判据是**要不要多实例**：单实例下进程内队列
比 Redis 少一次 RTT、少一个可用性依赖，而 Redis 默认 AOF everysec 的丢失窗口跟攒批是同量级。
真正需要 Redis 的场景是：多实例共享缓存失效/限流计数、配额原子扣减（这类**必须同步**，不能进异步管道）。

**加一个上游 key 选择策略**（如按会话亲和、按剩余额度）：在 `internal/relay/keyselector/` 实现 `Selector` 并 `Register`。
`Context` 里已经带了 channel/group/user/网关key/模型/协议/`AffinityKey`，够做亲和性。
两者的下拉框都会在设置页自动出现。

## API 一览

- 网关：`POST /v1/chat/completions`、`POST /v1/responses`、`POST /v1/messages`、`GET /v1/models`（用网关发放的 `sk-...`）
- 管理（Bearer JWT）：`/api/auth/*`、`/api/keys`、`/api/logs`、`/api/stats`
- 管理员：`/api/channels`、`/api/channels/:id/keys`、`/api/channel-keys/:id`、`/api/groups`、`/api/models`、`/api/models/:id/bindings`、`/api/bindings/:id`、`/api/settings`、`/api/users`
