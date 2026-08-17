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

## 架构：转发 / 控制 / 落库 解耦

同一个二进制，靠 `GATEWAY_ROLE` 切换职责。**目标是让转发实例变成无状态、可随时杀、
不持有写职责的东西**，于是它可以横向扩展，而控制台与落库不受影响。

| 角色 | 对外 | PostgreSQL | 职责 |
| --- | --- | --- | --- |
| `all`（默认） | `/v1/*` + `/api/*` + WebUI | 读写（池 32） | 单进程全功能，本地开发与小规模部署 |
| `gateway` | 只有 `/v1/*` | **只读，且只在启动/失效时读**（池 8） | 只转发，**可横向扩展** |
| `console` | 只有 `/api/*` + WebUI | 读写（池 32） | 管理台，不接转发流量 |
| `worker` | 只有 `/healthz` | 只写日志（池 32） | 消费日志队列 + 清理（需选主） |

```bash
make cluster   # 本地起 1 console(:8080) + 2 gateway(:8081/:8082) + 1 worker
```

为什么 gateway 的连接池要调小：N 个转发实例 × 32 连接会直接打穿 PG 默认的
`max_connections=100`。而它的热路径本来就不查库（内存快照），8 个连接绰绰有余。

### Redis 的职责边界

Redis 只做**跨实例才需要的事**，不碰热路径数据：

| 用途 | 说明 |
| --- | --- |
| 配置失效广播（Pub/Sub） | 管理台改完配置，各转发实例**立刻**重建本地快照。实测 0.3 秒内生效，此前要等 30 秒兜底轮询 |
| 单例任务选主 | 清理任务只能有一个执行者，否则 N 个实例并发删同一批数据 |
| 实例心跳 | 各实例把状态写进 Redis（带 TTL），管理台聚合成「集群」视图 |
| （后续）限流/配额/并发数 | 唯一必须同步、原子、跨实例的能力 |

**不交给 Redis 的**：配置快照本体仍在各实例本地内存（`atomic.Pointer`，纳秒级）。
Redis 只负责「通知失效」，不负责「存配置」——热路径不该有网络故障面。

### 故障语义：fail-open，但降级必须可见

Redis 挂了网关继续转发。这是显式选择：Redis 承担的是限流/协调，
**宁可超额，不可拒服务**。

| 能力 | Redis 不可用时 |
| --- | --- |
| 配置失效广播 | 退回 30 秒兜底轮询（配置变更最多迟 30 秒生效） |
| 限流/配额（后续） | **放过**（fail-open） |
| 清理任务选主 | **停止清理**（fail-**closed**：宁可不删，不可重复删） |

两个必要的配套，否则 fail-open 会变成「静默裸奔」：

1. **降级可见**：概览页「集群与协调」卡片显示 Redis 健康度、降级次数、最近错误。
   这跟日志丢弃计数是同一个原则——**隐性故障是不可接受的**。
2. **超时 + 熔断**：每次 Redis 调用独立超时（默认 50ms），连续失败打开熔断直接短路。
   因为**慢比挂危险**：彻底挂掉会立刻返回错误，反而安全；变慢会把转发一起拖死。

单实例（未配置 Redis）时一切照旧：选主进入 solo 模式直接视为 leader，
否则本地开发会永远不清理。

### 当前的已知缺口

- **gateway 角色转发的请求日志会被丢弃**。它已经不直连 PG 写日志了（这是解耦的目的），
  但 Redis Streams 队列还没接，所以日志暂时没有去处。集群视图里会用红色标出丢弃数，
  启动日志也会显式警告。需要完整日志时用 `role=all` 单实例部署。
- **原文归档功能已停用**（`store.ArchiveFeatureEnabled = false`）。它写本地磁盘，
  而请求落在哪个实例是不确定的，管理台根本读不到那个文件。代码全部保留，
  等换成共享存储（S3/MinIO）后再开。

## 存储选型：PostgreSQL + 磁盘归档

**结构化数据进 PostgreSQL，请求/响应原文进磁盘文件**，这是两类完全不同的数据：

| | 控制面（配置） | 观测面（日志） | 原文归档 |
| --- | --- | --- | --- |
| 存在哪 | PG | PG | 磁盘文件 `data/archive/` |
| 量级 | 几百行 | 随流量线性增长 | **增长最快** |
| 要求 | 事务、唯一约束、外键、改完立刻生效 | 追加即忘、时间窗口聚合、TTL | 能整体冷备/迁对象存储 |

原文**故意不进数据库**：它是膨胀最快的部分，放进去会拖着数据库一起变大，
而单独放文件就能按天目录直接打包搬走、或换成对象存储，跟数据库解耦。
而且原文归档**默认关闭**（设置页可开）——多数时候只在排查问题时才需要它。

用 PG 而不是 sqlite/MySQL 的具体原因：

- **多实例**。sqlite 只能单实例（全局单写者）；这是离开它的首要理由。
- **`jsonb`**。网关里有一批「结构不稳定、又不值得单独建列」的数据。典型是
  **上游返回的原始 usage 对象**：我们归一化了 3 个 token 数字，但 anthropic 还给
  `cache_creation_input_tokens`、responses 还给 `reasoning_tokens`，各家都在加。
  这些原样存 `request_logs.usage`（jsonb），要查就 `usage->>'reasoning_tokens'`，
  不用每来一个字段就改表。`channels.config` 同理，留给上游的扩展配置。
- **按事务控制持久化强度**。见下面的 `synchronous_commit`。
- **外键约束是打开的**。sqlite 下我关掉了（它加列要重建表，开外键会让迁移失败），PG 没这问题。
  注意 `request_logs` 上**故意没有外键**：日志是反规范化的历史快照，上游删了记录也要留下。

## 性能：热路径不落库

早期实现每个请求在 handler 里同步做 7 次 SELECT + 2 次文件写 + 3 次写事务（1 条日志 INSERT +
2 条 `last_used_at` UPDATE）。当时是 sqlite——**全局单写者**且每次 commit 一次 fsync，于是：

- 吞吐被压在 ~220 RPS，且 **c=1 和 c=50 的 RPS 一样**（说明是全局串行，加并发没用）
- p99 2.5s、单条 INSERT 最慢 4.4s（`busy_timeout` 是 5s，再高一点就开始静默丢日志）
- 网关自己记的 `duration_ms` 却是 p50=0ms —— 时间全花在响应发出**之后**的落库上

现在（同机、空转上游、非流式最小请求；PG 在本机 docker 里）：

| 场景 | RPS | p50 | p99 | max |
| --- | --- | --- | --- | --- |
| 直连上游（基线，不经网关） | 59,401 | 0.58ms | 4.6ms | 11ms |
| 同步落库时代 c=50（sqlite） | 219 | 41ms | 2.50s | 5.05s |
| **现在 c=1** | **4,156** | **0.20ms** | 0.93ms | 2.6ms |
| **现在 c=50** | **17,413** | 2.27ms | 12.3ms | 22.7ms |
| **现在 c=200** | **22,478** | 7.87ms | 24.0ms | 38.7ms |

吞吐约 **80 倍**，p99 好 **100 倍**。注意这个数字对 LLM 网关**没有实际意义**——真实请求要占着
上游连接好几秒，单实例几百 RPS 就到顶了。它的意义只是证明**落库不再是瓶颈**。四件事：

1. **`SET LOCAL synchronous_commit = off`，只对日志落库那个事务生效**。日志是观测数据，
   丢最近几个事务可以接受，换来不必等 WAL 落盘确认；而配置/用户的写入仍然是默认的
   `on`。（sqlite 的 `synchronous=NORMAL` 是全库开关，做不到这种区分——这是 PG 的净胜项）
2. **日志异步批量落库**（`internal/sink`）。请求只把日志行丢进有界队列；后台协程攒批，
   在**单个事务**里用 pgx 的 **COPY** 送进去。「每请求 3 个事务」变成「每批 1 个事务」，
   而且 COPY 走二进制流式协议，绕开 GORM 的逐行反射与 65535 绑定参数上限。
3. **`last_used_at` 合并**。这字段是 last-write-wins，天生可合并：每批只发 1 条
   `UPDATE ... WHERE id IN (...)`，而不是每请求 2 条。
4. **配置内存快照**（`internal/registry`）。网关 key、用户、归属、模型、绑定、上游 key
   全量缓存，用 `atomic.Pointer` 发布，热路径**零查询零锁**。任何管理 API 的写操作成功后
   由中间件同步重建快照（保证"改完立刻生效"），另有 30s 兜底刷新。

### 队列里只放日志行，不放原文

这是一条硬约束：**排队中的对象必须小且定长**。

早期实现把请求/响应原文一起塞进队列的 `Entry`，于是队列内存 = 队列深度 × 请求体大小：

| 每条请求/响应原文各 | 8192 深队列占用 |
| --- | --- |
| 500 B | 11.6 MB |
| 4 KB | 67.6 MB |
| 64 KB | **1,027 MB** |
| 1 MB | **16 GB** |

请求体上限是 20MB，所以理论最坏值是 8192 × 40MB = **320 GB**。这是个内存炸弹，
只是压测用空请求体所以一直没炸。

现在原文由 `archive` 包**在请求协程里当场写盘**，队列里只有日志行：
实测 **349 字节/条**，8192 深队列恒定 ≈ 2.7 MB，**与请求体大小无关**
（`TestQueueMemoryIndependentOfBodySize` 钉住了这个性质）。

为什么原文不需要排队：写文件只有 ~20µs（写进 page cache，不 fsync），相比动辄几秒的
LLM 调用可忽略；而攒批只对数据库写入有意义（减事务、减 WAL flush），对写文件没有任何收益。
顺带的好处是**原文不会因为队列满而丢**，而且日志行丢了原文还在。

实测证据：把攒批间隔临时设为 60s 让队列积压，灌入 3000 个 **1MB** 请求后，
队列里堆着 3000 条待落库日志，进程 RSS 60MB → **58MB（无增长）**；
旧实现在这里会是约 3GB。

### 怎么把丢弃降到 0

丢弃的根因是**消费端太慢**，不是缓冲太小——加大队列只是把问题藏起来，
抬高落库天花板才是真解。所以做了两件事：

**1. 用 COPY 代替逐行 INSERT。** 原先瓶颈在 GORM 的逐行反射序列化。同一张表、
同样单事务 + `synchronous_commit=off`，实测：

| 批大小 | GORM `CreateInBatches` | pgx `CopyFrom` | 提速 |
| --- | --- | --- | --- |
| 200 条 | 19,869 条/s | **102,794 条/s** | 5.2x |
| 1000 条 | 30,505 条/s | **188,090 条/s** | 6.2x |
| 5000 条 | 52,208 条/s | **203,303 条/s** | 3.9x |

**2. 队列按突发量定容**（默认 8192 → 32768）。因为每条只占 ~350 字节，
32768 条也只有 ~11MB。原来那个 8192 是按「每条 Entry 很大」拍的，摘掉原文后明显偏小了。

分离验证这两项各自的贡献（22,000 RPS，20000 请求砸在 0.9 秒内）：

| 配置 | 丢弃率 |
| --- | --- |
| 原文在队列里 + GORM + 队列 8192 | 26% |
| 摘掉原文 + GORM + 队列 8192 | 4.0% |
| 摘掉原文 + **COPY** + 队列 8192 | 2.95% |
| 摘掉原文 + **COPY** + 队列 **32768** | **0.00%** |

两个都需要：COPY 抬高稳态消费率，大队列吸收亚秒级突发。
50000 请求 / 500 并发（22,614 RPS）同样是 0 丢弃。

需要说明的是：**只要缓冲有界，就存在能压垮它的输入**。要在数学上"保证"不丢，
最后一步只能选阻塞（反压给客户端）或溢出到磁盘。但天花板抬到 20 万条/s 之后，
到达率远低于消费率，稳态下不会积压——对一个真实负载几百 RPS 的 LLM 网关，
这已经远超需要了。真出现持续丢弃，概览页的丢弃计数会明确告诉你。

**列清单漂移保护**：COPY 需要手写列顺序，而建表是按 `store.RequestLog` 结构体做的，
两边可能漂移（加了字段忘了同步 → 新字段静默不落库）。所以启动时用
`VerifyLogColumns` 跟数据库实际列比对，不一致就**退化回 GORM 路径**（慢但正确）
并打日志 + 在概览页显示 `using_copy=false`。

### 异步落库的代价（都是显式选择）

- **队列满会丢日志行**（不阻塞转发，也不影响原文归档）。当前实测**丢弃率 0.00%**，
  见下面「怎么把丢弃降到 0」。丢弃数、队列占用、上批耗时、以及是否退化出 COPY 快路径
  全部显示在概览页「异步落库管道」卡片里 —— **隐性丢数据是不可接受的，所以必须可见**。
- **日志有可见延迟**（默认 ≤200ms 或攒满 200 条）。攒批间隔/条数在设置页可调。
- **崩溃丢最后一批**。`SIGTERM` 优雅退出会先停 HTTP、再把队列刷完（实测入队/落库/丢弃 = 421/421/0）；
  `kill -9` 则丢最后一批。
- **改小攒批间隔要立刻生效**：后台有个 1 秒的守望定时器专门发现「间隔被调小了」，
  否则从 60s 改回 200ms 得等那 60s 走完（这是实测发现并修掉的 bug）。

### 流式响应不再攒内存

原先流式响应为了归档会把整个 SSE 攒在内存（每请求上限 8MB），20 个并发长回答就是 160MB。
改成**边转发边追加写文件**（64KB bufio）后实测 20 个并发 × 12MB 流式响应：
进程 RSS 仅 +19MB，归档文件完整 12.2MB 落盘。（归档功能目前已停用，但这个性质保留在代码里。）

概览页的统计只按**时间窗口**（1 小时 / 1 天）查询，不做全表 `COUNT/SUM`——
logs 表会一直增长，全表扫描的首页迟早会拖垮。

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
| 原文归档 | ~~请求原文 + 响应全文落本地文件~~ **已停用**（写本地盘，与多实例转发不兼容） |
| 清理服务 | 后台 goroutine 按保留天数（默认 7 天，WebUI 可改）删除历史日志；多实例下经 Redis 选主，只有一个实例执行 |

不做：计费/额度、渠道健康检查与重试、协议互转（openai ↔ anthropic 的 body 翻译）、embeddings/images 等其它端点。

## 快速开始

```bash
# 依赖：Go 1.22+ / Node 20+ / 一个 PostgreSQL 14+（Redis 可选，多实例才需要）
make db-up      # 用 docker compose 起 PG（库 gateway / gateway_test，账号 gateway/gateway）
cp .env.example .env   # 可选：改端口/密码/密钥；真实环境变量优先级高于 .env
make build      # 构建前端并打包成单个二进制 bin/gateway（前端已 embed）
./bin/gateway   # 访问 http://localhost:8080 ，用 admin / admin 登录
```

已经有 PG 的话不用 `make db-up`，建个库然后设 `GATEWAY_DB_*`（或整条 `GATEWAY_DB_DSN`）即可。
表结构由 `AutoMigrate` 自动创建，首次启动会写入默认归属 `default` 与管理员 `admin/admin`。

多实例（转发横向扩展）：

```bash
# .env 里配上 GATEWAY_REDIS_ADDR，然后
make cluster        # 1 console(:8080) + 2 gateway(:8081/:8082) + 1 worker
make cluster-down
```

管理台在 :8080，转发流量打 :8081 / :8082（前面通常再挂个负载均衡）。
在管理台改配置，两个 gateway 会通过 Redis 广播**立刻**生效。

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
  internal/config/                环境变量配置（含极简 .env 加载）
    role.go                       ★ 进程角色定义（all/gateway/console/worker）
  internal/store/                 GORM 模型 / PostgreSQL / 设置 KV（含默认值与缓存）
    jsonb.go                      ★ jsonb 列的最小封装（原始 JSON 直存直取）
  internal/storetest/             测试用的一次性 PG 环境（每个测试一个独立 schema）
  internal/auth/                  JWT、bcrypt、sk- key 生成
  internal/relay/
    protocol.go                   ★ 端点协议接口 + 注册表 + 共用小工具
    protocol_openai.go            openai-chat / openai-responses（Bearer）
    protocol_anthropic.go         anthropic-messages（x-api-key + anthropic-version）
    relay.go                      转发主流程（协议无关：选上游 → 按归属选 key → 改模型名 → 直转 → 落日志）
    selector/selector.go          ★ 上游绑定选择策略（random / weighted）
    keyselector/keyselector.go    ★ 上游 key 选择策略（random / weighted / affinity-hash）
                                  （relay 热路径只读 registry 快照 + 投递 sink，不碰数据库）
  internal/archive/               请求/响应原文归档（默认关闭；请求协程当场写盘）+ 按天清理
  internal/sink/                  ★ 日志异步批量落库（队列只放日志行，~350 字节/条）
    copy.go                       ★ pgx COPY 快路径（比 GORM 快 4~6 倍）+ 列清单漂移校验
  internal/registry/              ★ 配置内存快照，转发热路径零查询
  internal/rds/                   ★ Redis 封装：超时 + 熔断 + 降级可见（fail-open）
  internal/coord/                 ★ 多实例协调：配置失效广播 / 单例选主 / 实例心跳
  internal/cleaner/               后台清理服务
  internal/httpapi/               gin 路由、中间件、各资源 handler、静态前端挂载
  internal/web/                   //go:embed dist（前端产物）
web/                              Vite + React + TS + Tailwind v4 + shadcn 风格组件
scripts/mock_upstream.py          本地 mock 上游
```

## 数据与文件

- 结构化数据：PostgreSQL（`AutoMigrate` 建表，外键约束打开）
- 归档原文（**功能已停用**，见上面「当前的已知缺口」）：
  `data/archive/<YYYY-MM-DD>/<request-id>.request.json` 与 `<request-id>.response.txt`
  - `request.json` 含元信息（用户、key、模型、上游 URL）+ 客户端原始 body
  - `response.txt` 非流式为完整响应体，流式为完整 SSE 文本（边转发边追加写，单文件上限 32MB 后截断）
  - 在请求协程里**当场写盘**（不进异步队列：原文体积大，排队等于攒在堆上）
  - 非流式的响应归档写在回吐响应**之后**，不抬高客户端看到的首字节延迟
  - 日志页点「原文」即读取这两个文件；被清理后提示已删除

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `GATEWAY_PORT` | `8080` | 监听端口 |
| `GATEWAY_DATA_DIR` | `./data` | 数据目录（只存原文归档） |
| `GATEWAY_ARCHIVE_DIR` | `<data>/archive` | 归档目录 |
| `GATEWAY_JWT_SECRET` | `dev-insecure-...` | **生产务必修改** |
| `GATEWAY_ADMIN_USER/PASS` | `admin` / `admin` | 初始管理员（仅无 admin 时创建） |
| `GATEWAY_ALLOW_REGISTER` | `true` | 是否允许自助注册（也可在设置页改） |
| `GATEWAY_LOG_QUEUE_SIZE` | `32768` | 异步落库队列容量（每条 ~350 字节；满了丢日志行，不阻塞转发） |
| `GATEWAY_DB_DSN` | 由下面分量拼出 | 整条 PG 连接串，给了就优先用 |
| `GATEWAY_DB_HOST` / `_PORT` | `127.0.0.1` / `5432` | |
| `GATEWAY_DB_USER` / `_PASSWORD` / `_NAME` | `gateway` / `gateway` / `gateway` | |
| `GATEWAY_DB_SSLMODE` | `disable` | |
| `GATEWAY_DB_TIMEZONE` | `Asia/Shanghai` | |
| `GATEWAY_DB_MAX_OPEN` / `_MAX_IDLE` | `32` / `8` | 连接池；别超过 PG 的 `max_connections` |
| `GATEWAY_TEST_DSN` | 无 | `go test` 用的库；不设则跳过需要数据库的测试 |
| `GATEWAY_ROLE` | `all` | 进程角色：`all` / `gateway` / `console` / `worker` |
| `GATEWAY_INSTANCE_ID` | 主机名 | 多实例下区分心跳与日志 |
| `GATEWAY_REDIS_ADDR` | 空 | 不配则为单实例模式（无广播、无选主） |
| `GATEWAY_REDIS_PASSWORD` / `_DB` / `_PREFIX` | — / `0` / `gw` | |
| `GATEWAY_REDIS_TIMEOUT_MS` | `50` | 单次命令超时；慢比挂危险 |
| `GATEWAY_TEST_REDIS_ADDR` | 无 | 协调层测试用；不设则跳过 |

启动时会读当前目录的 `.env`（见 `.env.example`），**已存在的真实环境变量不会被覆盖**。

WebUI 可改的运行时配置：归档保留天数、日志保留天数、清理间隔、上游绑定路由策略、**上游 key 选择策略**、
**新用户默认归属**、上游超时、注册开关、**异步落库攒批间隔/条数**、**是否归档原文**。

`data/` 目录现在只放原文归档，数据库不在里面（早期版本是 sqlite `data/gateway.db`，已彻底移除，
不提供 sqlite → PG 的数据迁移）。

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

**接入 Redis Streams**：让 gateway 角色 `XADD` 进队列、worker 消费后批量写 PG。
这样转发实例既不连 PG 写库、也不丢日志，而且网关重启不丢（现在 `kill -9` 会丢最后一批）。
`sink.Sink` 是接口，现在的进程内实现原地留着当 Redis 不可用时的降级路径。

**限流 / 配额 / 并发数**：这是唯一「必须同步、原子、跨实例」的能力，只能靠 Redis
（`INCR`+`EXPIRE` 或 Lua 令牌桶）。对 LLM 网关**并发数限制比 RPS 更重要**——
请求要占着上游连接几十秒。fail-open，但要有本地兜底配额（全局配额 / 实例数），
免得一次 Redis 故障就让上游账单失控。

**上游 key 熔断冷却**：撞了 429/401 就 `SET cooldown EX 60`，所有实例立刻绕开这把 key。
现在是撞了也毫无反应，下一个请求还往同一把打。

**日志表按天分区**：`request_logs` 现在是普通表，保留策略靠 `DELETE`。上量后应改成
`PARTITION BY RANGE (created_at)` + 按天 `DROP PARTITION`——瞬间完成、不产生死行、不用等 autovacuum。
`AutoMigrate` 建不了分区表，需要手写 DDL + 预建分区（或上 `pg_partman`），所以先留作扩展点。

**把落库换成 Redis / ClickHouse**：`sink.Sink` 是接口（`Submit` / `Stats` / `Close`），
换一个 Redis Stream 实现即可，请求侧代码不动。判据是**要不要多实例**：单实例下进程内队列
比 Redis 少一次 RTT、少一个可用性依赖，而 Redis 默认 AOF everysec 的丢失窗口跟攒批是同量级。
真正需要 Redis 的场景是：多实例共享缓存失效/限流计数、配额原子扣减（这类**必须同步**，不能进异步管道）。
日志真到亿级、要做用量报表时，把 `request_logs` 单独挪去 ClickHouse（配置仍留 PG）。

**加一个上游 key 选择策略**（如按会话亲和、按剩余额度）：在 `internal/relay/keyselector/` 实现 `Selector` 并 `Register`。
`Context` 里已经带了 channel/group/user/网关key/模型/协议/`AffinityKey`，够做亲和性。
两者的下拉框都会在设置页自动出现。

## API 一览

- 网关：`POST /v1/chat/completions`、`POST /v1/responses`、`POST /v1/messages`、`GET /v1/models`（用网关发放的 `sk-...`）
- 管理（Bearer JWT）：`/api/auth/*`、`/api/keys`、`/api/logs`、`/api/stats`
- 管理员：`/api/channels`、`/api/channels/:id/keys`、`/api/channel-keys/:id`、`/api/groups`、`/api/models`、`/api/models/:id/bindings`、`/api/bindings/:id`、`/api/settings`、`/api/users`
