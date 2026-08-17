import { useCallback, useEffect, useState } from 'react'
import { toast } from 'sonner'
import { api, type ProtocolInfo, type Stats } from '@/lib/api'
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

      {sink && (
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              异步落库管道
              {sink.dropped > 0 ? <Badge variant="destructive">有丢弃</Badge> : <Badge variant="success">健康</Badge>}
              {sink.using_copy ? (
                <Badge variant="outline">COPY 快路径</Badge>
              ) : (
                <Badge variant="destructive">已退化为逐行 INSERT</Badge>
              )}
            </CardTitle>
            <CardDescription>
              请求只把一行日志（约 400 字节）丢进队列，后台攒批单事务落库；队列占用与请求体大小无关。
              队列满会丢<b>日志行</b>（不阻塞转发、不影响原文归档），所以这里必须能看到。
            </CardDescription>
          </CardHeader>
          <CardContent>
            <div className="grid gap-3 text-sm sm:grid-cols-3 lg:grid-cols-4">
              <Metric label="队列占用" value={`${sink.queue_len} / ${sink.queue_cap}`} />
              <Metric label="累计入队" value={sink.enqueued.toLocaleString()} />
              <Metric label="累计落库" value={sink.persisted.toLocaleString()} />
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
            {!sink.using_copy && (
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
