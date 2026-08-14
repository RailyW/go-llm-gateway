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
    archive.go                    请求/响应原文归档与按天清理
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
  - `response.txt` 非流式为完整响应体，流式为完整 SSE 文本（单文件上限 8MB）
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

WebUI 可改的运行时配置：归档保留天数、日志保留天数、清理间隔、上游绑定路由策略、**上游 key 选择策略**、
**新用户默认归属**、上游超时、注册开关。

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

**加一个上游 key 选择策略**（如按会话亲和、按剩余额度）：在 `internal/relay/keyselector/` 实现 `Selector` 并 `Register`。
`Context` 里已经带了 channel/group/user/网关key/模型/协议/`AffinityKey`，够做亲和性。
两者的下拉框都会在设置页自动出现。

## API 一览

- 网关：`POST /v1/chat/completions`、`POST /v1/responses`、`POST /v1/messages`、`GET /v1/models`（用网关发放的 `sk-...`）
- 管理（Bearer JWT）：`/api/auth/*`、`/api/keys`、`/api/logs`、`/api/stats`
- 管理员：`/api/channels`、`/api/channels/:id/keys`、`/api/channel-keys/:id`、`/api/groups`、`/api/models`、`/api/models/:id/bindings`、`/api/bindings/:id`、`/api/settings`、`/api/users`
