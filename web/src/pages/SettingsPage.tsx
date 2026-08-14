import { useEffect, useState } from 'react'
import { toast } from 'sonner'
import { Play, Save } from 'lucide-react'
import { api, type CleanerStatus } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge, Label, Select, Switch } from '@/components/ui/misc'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'

export default function SettingsPage() {
  const [s, setS] = useState<Record<string, string>>({})
  const [strategies, setStrategies] = useState<string[]>([])
  const [cleaner, setCleaner] = useState<CleanerStatus | null>(null)
  const [busy, setBusy] = useState(false)

  const load = () =>
    api
      .settings()
      .then((r) => {
        setS(r.settings)
        setStrategies(r.strategies)
        setCleaner(r.cleaner)
      })
      .catch((e) => toast.error((e as Error).message))

  useEffect(() => {
    load()
  }, [])

  const set = (k: string, v: string) => setS((p) => ({ ...p, [k]: v }))

  const save = async () => {
    setBusy(true)
    try {
      const r = await api.updateSettings(s)
      setS(r.settings)
      toast.success('已保存')
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const runCleanup = async () => {
    try {
      const r = await api.runCleanup()
      setCleaner(r.cleaner)
      toast.success('清理已执行')
    } catch (e) {
      toast.error((e as Error).message)
    }
  }

  const num = (k: string, label: string, hint: string) => (
    <div className="space-y-1.5">
      <Label>{label}</Label>
      <Input type="number" min={0} value={s[k] ?? ''} onChange={(e) => set(k, e.target.value)} />
      <p className="text-xs text-muted-foreground">{hint}</p>
    </div>
  )

  return (
    <div className="space-y-4">
      <div className="flex items-end justify-between">
        <div>
          <h1 className="text-xl font-semibold">设置</h1>
          <p className="text-sm text-muted-foreground">清理策略、路由策略、注册开关</p>
        </div>
        <Button onClick={save} disabled={busy}>
          <Save className="size-4" /> 保存
        </Button>
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>数据保留</CardTitle>
            <CardDescription>后台清理服务按此配置删除过期数据</CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            {num('archive_retention_days', '原文归档保留天数', '本地存储的请求/响应全文，超过天数的整天目录会被删除。0 = 不清理')}
            {num('log_retention_days', 'logs 表保留天数', '数据库里的结构化日志。0 = 永久保留')}
            {num('cleanup_interval_minutes', '清理间隔（分钟）', '后台清理服务的运行周期，修改后下一轮生效')}
            <div className="flex items-center gap-2">
              <Button variant="outline" size="sm" onClick={runCleanup}>
                <Play className="size-4" /> 立即清理一次
              </Button>
              {cleaner?.running && <Badge>运行中</Badge>}
            </div>
            {cleaner && (
              <div className="space-y-1 rounded-md bg-muted p-3 text-xs text-muted-foreground">
                <div>上次运行：{cleaner.last_run_at ? new Date(cleaner.last_run_at).toLocaleString() : '—'}</div>
                <div>下次运行：{cleaner.next_run_at ? new Date(cleaner.next_run_at).toLocaleString() : '—'}</div>
                <div>
                  上次删除：{cleaner.last_removed_archive_dirs} 个归档目录 / {cleaner.last_removed_log_rows} 条日志
                </div>
                {cleaner.last_error && <div className="text-destructive">错误：{cleaner.last_error}</div>}
              </div>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>转发与账号</CardTitle>
            <CardDescription>多绑定路由策略与注册开关</CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="space-y-1.5">
              <Label>路由策略</Label>
              <Select value={s.route_strategy ?? 'random'} onChange={(e) => set('route_strategy', e.target.value)}>
                {strategies.map((n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
                ))}
              </Select>
              <p className="text-xs text-muted-foreground">一个模型有多个上游绑定时的选择方式（random 随机 / weighted 按权重）</p>
            </div>
            {num('upstream_timeout_seconds', '上游超时（秒）', '非流式请求的超时；流式请求不设整体超时')}
            <div className="flex items-center justify-between rounded-md border border-border p-3">
              <div>
                <Label>允许自助注册</Label>
                <p className="text-xs text-muted-foreground">关闭后只能由管理员创建/启用账号</p>
              </div>
              <Switch
                checked={(s.allow_register ?? 'true') === 'true'}
                onCheckedChange={(v) => set('allow_register', v ? 'true' : 'false')}
              />
            </div>
          </CardContent>
        </Card>
      </div>
    </div>
  )
}
