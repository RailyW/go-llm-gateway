import { useCallback, useEffect, useState } from 'react'
import { toast } from 'sonner'
import { api, type Peer, type ProtocolInfo, type Stats } from '@/lib/api'
import { useAuth } from '@/lib/auth'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge, Label } from '@/components/ui/misc'

const WINDOWS: { key: '1h' | '24h'; label: string }[] = [
  { key: '1h', label: '1 小时内' },
  { key: '24h', label: '1 天内' },
]

export default function DashboardPage() {
  const { user } = useAuth()
  const [stats, setStats] = useState<Stats | null>(null)
  const [window, setWindow] = useState<'1h' | '24h'>('1h')
  const [protocols, setProtocols] = useState<ProtocolInfo[]>([])
  const [oldPw, setOldPw] = useState('')
  const [newPw, setNewPw] = useState('')

  const load = useCallback(() => {
    api.stats(window).then(setStats).catch((e) => toast.error((e as Error).message))
  }, [window])

  useEffect(() => {
    load()
  }, [load])
  useEffect(() => {
    api.meta().then((m) => setProtocols(m.protocols)).catch(() => {})
  }, [])

  const cards = [
    { label: '请求数', value: stats?.requests ?? 0 },
    { label: '失败请求', value: stats?.errors ?? 0 },
    { label: 'Prompt tokens', value: stats?.prompt_tokens ?? 0 },
    { label: 'Completion tokens', value: stats?.completion_tokens ?? 0 },
  ]
  const totals = [
    { label: '我的 Key', value: stats?.keys ?? 0 },
    ...(user?.role === 'admin'
      ? [
          { label: '上游数', value: stats?.channels ?? 0 },
          { label: '模型数', value: stats?.models ?? 0 },
          { label: '归属数', value: stats?.groups ?? 0 },
          { label: '用户数', value: stats?.users ?? 0 },
        ]
      : []),
  ]

  const changePw = async (e: React.FormEvent) => {
    e.preventDefault()
    try {
      await api.changePassword(oldPw, newPw)
      toast.success('密码已修改')
      setOldPw('')
      setNewPw('')
    } catch (err) {
      toast.error((err as Error).message)
    }
  }

  const sink = stats?.sink
  const consumer = stats?.consumer
  const base = location.origin
  const dropRate = sink && sink.enqueued + sink.dropped > 0 ? (sink.dropped / (sink.enqueued + sink.dropped)) * 100 : 0

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">概览</h1>
          <p className="text-sm text-muted-foreground">欢迎回来，{user?.username}</p>
        </div>
        <div className="flex items-center gap-1 rounded-md border border-border p-1">
          {WINDOWS.map((w) => (
            <Button
              key={w.key}
              size="sm"
              variant={window === w.key ? 'default' : 'ghost'}
              onClick={() => setWindow(w.key)}
            >
              {w.label}
            </Button>
          ))}
        </div>
      </div>

      <div>
        <div className="mb-2 text-xs text-muted-foreground">
          调用统计 · {WINDOWS.find((w) => w.key === window)?.label}
          （不做全量统计：logs 表会一直增长，全表扫描的概览接口会越来越慢）
        </div>
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          {cards.map((c) => (
            <Card key={c.label}>
              <CardContent className="p-5">
                <div className="text-xs text-muted-foreground">{c.label}</div>
                <div className="mt-1 text-2xl font-semibold tabular-nums">{c.value.toLocaleString()}</div>
              </CardContent>
            </Card>
          ))}
        </div>
      </div>

      <div className="grid gap-4 sm:grid-cols-3 lg:grid-cols-5">
        {totals.map((c) => (
          <Card key={c.label}>
            <CardContent className="p-4">
              <div className="text-xs text-muted-foreground">{c.label}</div>
              <div className="mt-1 text-xl font-semibold tabular-nums">{c.value.toLocaleString()}</div>
            </CardContent>
          </Card>
        ))}
      </div>

      {(stats?.instance || stats?.redis?.enabled) && (
        <Card>
          <CardHeader>
            <CardTitle className="flex flex-wrap items-center gap-2">
              集群与协调
              {stats?.instance && <Badge variant="outline">本机 {stats.instance.id} · {stats.instance.role}</Badge>}
              {stats?.redis?.enabled ? (
                stats.redis.healthy ? (
                  <Badge variant="success">Redis 正常</Badge>
                ) : (
                  <Badge variant="destructive">Redis 降级中</Badge>
                )
              ) : (
                <Badge variant="outline">未配置 Redis（单实例）</Badge>
              )}
            </CardTitle>
            <CardDescription>
              转发（gateway）可横向扩展；管理台（console）与后台（worker）各自独立。
              Redis 只负责跨实例的配置失效广播与单例任务选主，配置快照仍在各实例本地内存里（热路径零网络）。
              <b>限流/配额等准入控制在 Redis 不可用时 fail-open（放过）</b>，清理任务则 fail-closed（宁可不删）。
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            {stats?.redis?.enabled && (
              <div className="grid gap-3 text-sm sm:grid-cols-3 lg:grid-cols-4">
                <Metric label="Redis 地址" value={stats.redis.addr || '—'} />
                <Metric label="调用 / 失败" value={`${stats.redis.calls} / ${stats.redis.failures}`} bad={stats.redis.failures > 0} />
                <Metric label="降级次数" value={String(stats.redis.degradations)} bad={stats.redis.degradations > 0} />
                <Metric
                  label="配置广播"
                  value={
                    stats.invalidate && (stats.invalidate as Record<string, unknown>).enabled
                      ? `已发 ${(stats.invalidate as Record<string, number>).published ?? 0} / 收 ${(stats.invalidate as Record<string, number>).received ?? 0}`
                      : '未启用'
                  }
                />
              </div>
            )}
            {stats?.redis?.last_error && (
              <p className="text-xs text-destructive">Redis 最近错误：{stats.redis.last_error}</p>
            )}
            {stats?.cluster && stats.cluster.length > 0 && (
              <div className="space-y-2">
                <div className="text-xs text-muted-foreground">
                  存活实例（心跳 15 秒过期）。多实例下本页其余指标只反映当前这台，集群情况看这里：
                </div>
                <div className="overflow-x-auto">
                  <table className="w-full text-xs">
                    <thead className="text-muted-foreground">
                      <tr className="border-b border-border">
                        <th className="py-1.5 text-left font-medium">实例</th>
                        <th className="py-1.5 text-left font-medium">角色</th>
                        <th className="py-1.5 text-left font-medium">日志去处</th>
                        <th className="py-1.5 text-right font-medium">已处理</th>
                        <th className="py-1.5 text-right font-medium">丢弃</th>
                        <th className="py-1.5 text-right font-medium">积压</th>
                        <th className="py-1.5 text-right font-medium">心跳</th>
                      </tr>
                    </thead>
                    <tbody>
                      {[...stats.cluster]
                        .sort((a, b) => a.instance.localeCompare(b.instance))
                        .map((p: Peer) => (
                          <tr key={p.instance} className="border-b border-border/50">
                            <td className="py-1.5 font-medium">{p.instance}</td>
                            <td className="py-1.5">
                              <Badge variant="outline">{p.role}</Badge>
                            </td>
                            <td className="py-1.5 text-xs text-muted-foreground">
                              {p.sink?.via === 'redis-stream'
                                ? 'Redis Stream'
                                : p.sink?.via === 'postgres'
                                  ? 'PostgreSQL' + (p.consumer ? ' + 消费 Stream' : '')
                                  : '不处理'}
                            </td>
                            <td className="py-1.5 text-right tabular-nums">
                              {p.consumer
                                ? p.consumer.persisted.toLocaleString()
                                : p.sink?.active
                                  ? p.sink.persisted.toLocaleString()
                                  : '—'}
                            </td>
                            <td
                              className={`py-1.5 text-right tabular-nums ${
                                (p.sink?.dropped ?? 0) > 0 ? 'text-destructive font-medium' : ''
                              }`}
                            >
                              {p.sink?.dropped ?? 0}
                            </td>
                            <td
                              className={`py-1.5 text-right tabular-nums ${
                                (p.consumer?.backlog ?? 0) > 10000 ? 'text-destructive font-medium' : ''
                              }`}
                            >
                              {p.consumer ? p.consumer.backlog.toLocaleString() : '—'}
                            </td>
                            <td className="py-1.5 text-right text-muted-foreground">
                              {p.at ? new Date(p.at).toLocaleTimeString() : '—'}
                            </td>
                          </tr>
                        ))}
                    </tbody>
                  </table>
                </div>
                {stats.cluster.some((p) => (p.sink?.dropped ?? 0) > 0) && (
                  <p className="text-xs text-destructive">
                    有实例在丢日志。常见原因：本地缓冲被打满（瞬时流量远超攒批速度），
                    或 Redis 不可用导致 XADD 失败（fail-open：宁可丢观测数据也不阻塞转发）。
                  </p>
                )}
                {stats.cluster.every((p) => !p.consumer) && stats.cluster.some((p) => p.sink?.via === 'redis-stream') && (
                  <p className="text-xs text-destructive">
                    有 gateway 实例在往 Redis Stream 投日志，但集群里没有任何实例在消费（缺 worker/all）。
                    日志会一直堆到流上限然后被丢弃 —— 请启动一个 <code>GATEWAY_ROLE=worker</code> 实例。
                  </p>
                )}
              </div>
            )}
          </CardContent>
        </Card>
      )}

      {consumer && (
        <Card>
          <CardHeader>
            <CardTitle className="flex flex-wrap items-center gap-2">
              日志消费端（Redis Stream → PostgreSQL）
              {consumer.running ? <Badge variant="success">运行中</Badge> : <Badge variant="destructive">未运行</Badge>}
              {consumer.backlog > 10000 && <Badge variant="destructive">积压偏高</Badge>}
              {consumer.failed > 0 && <Badge variant="destructive">有落库失败</Badge>}
              {consumer.poisoned > 0 && <Badge variant="destructive">有丢弃</Badge>}
            </CardTitle>
            <CardDescription>
              从 <code>{consumer.stream_key}</code> 读一批 → 落库成功 → 才 <code>XACK</code>。
              顺序反过来（先 ACK 再落库）的话，worker 崩溃时那批数据一样会丢，
              Redis Stream 就退化成一个更慢的进程内队列了。
              <b>未 ACK 的消息会被 XAUTOCLAIM 接管重试</b>，所以杀 worker 不丢日志。
            </CardDescription>
          </CardHeader>
          <CardContent>
            <div className="grid gap-3 text-sm sm:grid-cols-3 lg:grid-cols-4">
              <Metric label="积压 (lag)" value={consumer.backlog.toLocaleString()} bad={consumer.backlog > 10000} />
              <Metric label="未确认 (pending)" value={consumer.pending.toLocaleString()} bad={consumer.pending > 5000} />
              <Metric label="累计消费" value={consumer.consumed.toLocaleString()} />
              <Metric label="累计落库" value={consumer.persisted.toLocaleString()} />
              <Metric label="重试接管" value={consumer.retried.toLocaleString()} />
              <Metric label="毒消息丢弃" value={consumer.poisoned.toLocaleString()} bad={consumer.poisoned > 0} />
              <Metric label="落库失败批次" value={consumer.failed.toLocaleString()} bad={consumer.failed > 0} />
              <Metric label="上批 / 耗时" value={`${consumer.last_len} 条 / ${consumer.last_ms}ms`} />
            </div>
            {consumer.last_error && <p className="mt-3 text-xs text-destructive">上批错误：{consumer.last_error}</p>}
            <p className="mt-3 text-xs text-muted-foreground">
              流物理长度 {consumer.length.toLocaleString()} 条（已回收 {consumer.trimmed.toLocaleString()} 条）。
              <code>XACK</code> 不删消息，所以已落库的部分会被定期 <code>XTRIM</code> 回收 ——
              Redis 内存要留给限流计数那类真正需要它的东西。积压看 lag，别看物理长度。
              {!consumer.using_copy && ' COPY 快路径未启用，落库吞吐约降为 1/5。'}
            </p>
          </CardContent>
        </Card>
      )}

      {sink?.active && (
        <Card>
          <CardHeader>
            <CardTitle className="flex flex-wrap items-center gap-2">
              日志管道
              {sink.dropped > 0 ? <Badge variant="destructive">有丢弃</Badge> : <Badge variant="success">健康</Badge>}
              {sink.via === 'redis-stream' ? (
                <Badge variant="outline">投递到 Redis Stream</Badge>
              ) : (
                <Badge variant="outline">直接落库</Badge>
              )}
              {sink.via !== 'redis-stream' &&
                (sink.using_copy ? (
                  <Badge variant="outline">COPY 快路径</Badge>
                ) : (
                  <Badge variant="destructive">已退化为逐行 INSERT</Badge>
                ))}
            </CardTitle>
            <CardDescription>
              {sink.via === 'redis-stream' ? (
                <>
                  请求只把一行日志（约 400 字节）丢进本地缓冲，后台攒批 <code>XADD</code> 进 Redis Stream，
                  由 worker 实例落库。<b>请求协程不碰网络</b>——直接 XADD 等于把「同步写库」换成「同步写 Redis」，
                  热路径又多一个能抖动的依赖。
                </>
              ) : (
                <>
                  请求只把一行日志（约 400 字节）丢进队列，后台攒批单事务落库；队列占用与请求体大小无关。
                  队列满会丢<b>日志行</b>（不阻塞转发），所以这里必须能看到。
                </>
              )}
            </CardDescription>
          </CardHeader>
          <CardContent>
            <div className="grid gap-3 text-sm sm:grid-cols-3 lg:grid-cols-4">
              <Metric label="缓冲占用" value={`${sink.queue_len} / ${sink.queue_cap}`} />
              <Metric label="累计入队" value={sink.enqueued.toLocaleString()} />
              <Metric
                label={sink.via === 'redis-stream' ? '累计投递' : '累计落库'}
                value={sink.persisted.toLocaleString()}
              />
              <Metric
                label="累计丢弃"
                value={`${sink.dropped.toLocaleString()}${dropRate > 0 ? ` (${dropRate.toFixed(2)}%)` : ''}`}
                bad={sink.dropped > 0}
              />
              <Metric label="批次数" value={sink.batches.toLocaleString()} />
              <Metric label="上批条数" value={String(sink.last_batch_len)} />
              <Metric label="上批耗时" value={`${sink.last_flush_ms}ms`} />
              <Metric
                label="上次刷新"
                value={sink.last_flush_at ? new Date(sink.last_flush_at).toLocaleTimeString() : '—'}
              />
            </div>
            {sink.last_error && <p className="mt-3 text-xs text-destructive">上批错误：{sink.last_error}</p>}
            {!sink.using_copy && sink.via !== 'redis-stream' && (
              <p className="mt-3 text-xs text-destructive">
                COPY 快路径未启用，落库吞吐约降为 1/5。通常是 request_logs 的列与代码里的 COPY
                列清单不一致（给日志加了字段但没同步 sink/copy.go 的 logColumns），详见服务端日志。
              </p>
            )}
            {stats?.registry && (
              <p className="mt-3 text-xs text-muted-foreground">
                配置快照：{stats.registry.models} 个模型 / {stats.registry.callers} 个网关 key / {stats.registry.key_sets}{' '}
                组上游 key，已重建 {stats.registry.reloads} 次（转发热路径零查询）
              </p>
            )}
          </CardContent>
        </Card>
      )}

      {sink && !sink.active && (
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              日志管道
              {sink.dropped > 0 ? (
                <Badge variant="destructive">日志被丢弃</Badge>
              ) : (
                <Badge variant="outline">本实例不处理日志</Badge>
              )}
            </CardTitle>
            <CardDescription>
              {stats?.instance?.role === 'gateway' ? (
                <span className="text-destructive">
                  本实例是 gateway 角色但没有配置 Redis。它不直连数据库写日志（那是解耦的目的），
                  而没有 Redis 就没有别的去处 —— 转发正常，但请求日志会被丢弃。
                  请配置 <code>GATEWAY_REDIS_ADDR</code>，或改用 <code>GATEWAY_ROLE=all</code>。
                </span>
              ) : (
                <>
                  当前角色（{stats?.instance?.role}）不转发流量也不处理日志。
                  落库情况请看上面「集群与协调」里 worker/all 实例那几行。
                </>
              )}
            </CardDescription>
          </CardHeader>
        </Card>
      )}

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>调用方式</CardTitle>
            <CardDescription>同协议直转：打哪个端点就转到上游同名端点，不做协议翻译</CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="space-y-1.5">
              {protocols.map((p) => (
                <div key={p.name} className="flex items-center gap-2 text-xs">
                  <Badge variant="outline">{p.vendor}</Badge>
                  <code className="rounded bg-muted px-1.5 py-0.5">
                    POST {base}
                    {p.path}
                  </code>
                  <span className="text-muted-foreground">{p.label}</span>
                </div>
              ))}
            </div>
            <pre className="overflow-x-auto rounded-md bg-muted p-3 text-xs leading-relaxed">
{`# OpenAI chat/completions
curl ${base}/v1/chat/completions \\
  -H "Authorization: Bearer sk-你的网关key" \\
  -d '{"model":"你录入的模型名","messages":[{"role":"user","content":"hi"}]}'

# Anthropic messages（原生协议，鉴权头也用网关 key）
curl ${base}/v1/messages \\
  -H "x-api-key: sk-你的网关key" \\
  -H "anthropic-version: 2023-06-01" \\
  -d '{"model":"你录入的模型名","max_tokens":64,
       "messages":[{"role":"user","content":"hi"}]}'`}
            </pre>
            <p className="text-xs text-muted-foreground">
              模型列表：<code className="rounded bg-muted px-1">GET {base}/v1/models</code>
            </p>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>修改密码</CardTitle>
          </CardHeader>
          <CardContent>
            <form className="space-y-3" onSubmit={changePw}>
              <div className="space-y-1.5">
                <Label htmlFor="op">原密码</Label>
                <Input id="op" type="password" value={oldPw} onChange={(e) => setOldPw(e.target.value)} required />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="np">新密码</Label>
                <Input id="np" type="password" value={newPw} onChange={(e) => setNewPw(e.target.value)} required />
              </div>
              <Button type="submit" size="sm">
                保存
              </Button>
            </form>
          </CardContent>
        </Card>
      </div>
    </div>
  )
}

function Metric({ label, value, bad }: { label: string; value: string; bad?: boolean }) {
  return (
    <div className="rounded-md bg-muted px-3 py-2">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className={`mt-0.5 font-medium tabular-nums ${bad ? 'text-destructive' : ''}`}>{value}</div>
    </div>
  )
}
