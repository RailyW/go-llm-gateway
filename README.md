# go-llm-gateway

一个**最小可用**的 LLM 网关转发服务（对标 new-api 的极简版），Go + React。

只做核心闭环：**上游录入 → 模型录入 → 模型/上游绑定 → 发放自己的 API Key → 转发 `/v1/chat/completions`**。

## 功能范围

| 模块 | 说明 |
| --- | --- |
| 用户 | 注册 / 登录（JWT），角色 `admin` / `user`，首次启动自带 `admin / admin` |
| 上游 (Channel) | `base_url` + `api_key` + 协议类型（当前只有 `openai` 兼容） |
| 模型 (Model) | 对外暴露的模型名（客户端请求里的 `model`） |
| 绑定 (Binding) | 模型 → 上游 + **上游真实模型名**，支持一个模型绑多个上游 |
| 路由策略 | `random`（默认）/ `weighted`，可在设置页切换，接口可扩展 |
| API Key | 网关自己发放的 `sk-...`，归属用户，可停用/删除 |
| 网关端点 | `POST /v1/chat/completions`（含 **SSE 流式**）、`GET /v1/models` |
| 日志 | `logs` 表：用户、模型、上游、tokens、耗时、状态码、错误 |
| 原文归档 | 每次请求的**请求原文 + 响应全文**落本地文件，文件名 = 日志的 request id |
| 清理服务 | 后台 goroutine 按保留天数（默认 7 天，WebUI 可改）删除归档与历史日志 |

不做：计费/额度、渠道健康检查与重试、除 chat/completions 之外的端点、协议互转（已留接口）。

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

联调步骤：`上游` 页新增 → `模型` 页新增 → 展开模型点「绑定」→ `API Key` 页新建 key → 调用：

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-你的网关key" \
  -H "Content-Type: application/json" \
  -d '{"model":"你录入的模型名","stream":true,
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
    adapter.go                    ★ 上游协议适配器接口 + 注册表
    adapter_openai.go             openai 兼容实现（透传）
    relay.go                      转发主流程（选上游 → 改模型名 → 转发 → 流式/非流式 → 落日志）
    archive.go                    请求/响应原文归档与按天清理
    selector/selector.go          ★ 路由策略接口 + random / weighted
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

WebUI 可改的运行时配置：归档保留天数、日志保留天数、清理间隔、路由策略、上游超时、注册开关。

## 扩展点

**加一个上游协议**（如 anthropic 原生）：在 `internal/relay/` 新建 `adapter_anthropic.go`，实现 `Adapter` 接口的 5 个方法并在 `init()` 里 `Register(&AnthropicAdapter{})`；上游录入时把「协议类型」选成 `anthropic` 即可，转发主流程无需修改。

```go
type Adapter interface {
    Name() string
    BuildRequest(ctx, ch *store.Channel, req *ChatRequest) (*http.Request, error) // 构 URL/鉴权/请求体转换
    TransformResponse(status int, body []byte) (int, []byte, error)               // 非流式响应 → OpenAI 格式
    TransformStreamLine(line []byte) ([][]byte, error)                            // SSE 单行 → OpenAI 格式
    ExtractUsage(payload []byte) Usage                                            // 抽 token 用量
}
```

**加一个路由策略**（如轮询、最少失败）：在 `internal/relay/selector/` 实现 `Selector` 并 `Register`，设置页的下拉框会自动出现。

## API 一览

- 网关：`POST /v1/chat/completions`、`GET /v1/models`（Bearer `sk-...`）
- 管理（Bearer JWT）：`/api/auth/*`、`/api/keys`、`/api/logs`、`/api/stats`
- 管理员：`/api/channels`、`/api/models`、`/api/models/:id/bindings`、`/api/bindings/:id`、`/api/settings`、`/api/users`
