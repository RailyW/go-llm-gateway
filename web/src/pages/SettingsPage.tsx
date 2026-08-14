import { useEffect, useState } from 'react'
import { toast } from 'sonner'
import { Pencil, Play, Plus, Save, Trash2 } from 'lucide-react'
import { api, type CleanerStatus, type Group } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge, Label, Select, Switch } from '@/components/ui/misc'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Table, TableBody, TableCell, TableEmpty, TableHead, TableHeader, TableRow } from '@/components/ui/table'

export default function SettingsPage() {
  const [s, setS] = useState<Record<string, string>>({})
  const [strategies, setStrategies] = useState<string[]>([])
  const [keyStrategies, setKeyStrategies] = useState<string[]>([])
  const [groups, setGroups] = useState<Group[]>([])
  const [groupOpen, setGroupOpen] = useState(false)
  const [editingGroup, setEditingGroup] = useState<Group | null>(null)
  const [groupForm, setGroupForm] = useState({ name: '', remark: '' })
  const [cleaner, setCleaner] = useState<CleanerStatus | null>(null)
  const [busy, setBusy] = useState(false)

  const load = () =>
    api
      .settings()
      .then((r) => {
        setS(r.settings)
        setStrategies(r.strategies)
        setKeyStrategies(r.key_strategies)
        setCleaner(r.cleaner)
      })
      .catch((e) => toast.error((e as Error).message))

  const loadGroups = () => api.groups().then(setGroups).catch((e) => toast.error((e as Error).message))

  useEffect(() => {
    load()
    loadGroups()
  }, [])

  const openCreateGroup = () => {
    setEditingGroup(null)
    setGroupForm({ name: '', remark: '' })
    setGroupOpen(true)
  }
  const openEditGroup = (g: Group) => {
    setEditingGroup(g)
    setGroupForm({ name: g.name, remark: g.remark })
    setGroupOpen(true)
  }
  const saveGroup = async (e: React.FormEvent) => {
    e.preventDefault()
    try {
      if (editingGroup) await api.updateGroup(editingGroup.id, groupForm)
      else await api.createGroup(groupForm)
      toast.success('已保存')
      setGroupOpen(false)
      loadGroups()
    } catch (err) {
      toast.error((err as Error).message)
    }
  }
  const removeGroup = async (g: Group) => {
    if (!confirm(`删除归属「${g.name}」？`)) return
    try {
      await api.deleteGroup(g.id)
      toast.success('已删除')
      loadGroups()
    } catch (err) {
      toast.error((err as Error).message)
    }
  }

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
          <p className="text-sm text-muted-foreground">用户归属枚举、清理策略、路由与 key 选择策略、注册开关</p>
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
            <div className="space-y-1.5">
              <Label>上游 Key 选择策略</Label>
              <Select
                value={s.upstream_key_strategy ?? 'random'}
                onChange={(e) => set('upstream_key_strategy', e.target.value)}
              >
                {keyStrategies.map((n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
                ))}
              </Select>
              <p className="text-xs text-muted-foreground">
                同一上游、同一归属下配了多把 key 时怎么挑：random 随机 / weighted 按权重 / affinity-hash 同一网关 key 固定粘一把
              </p>
            </div>
            <div className="space-y-1.5">
              <Label>新用户默认归属</Label>
              <Select value={s.default_group_id ?? '1'} onChange={(e) => set('default_group_id', e.target.value)}>
                {groups.map((g) => (
                  <option key={g.id} value={String(g.id)}>
                    {g.name}
                  </option>
                ))}
              </Select>
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

      <Card>
        <CardHeader className="flex-row items-center justify-between">
          <div className="space-y-1">
            <CardTitle>用户归属（部门）</CardTitle>
            <CardDescription>
              归属是「用户 ↔ 上游 key」的桥梁：用户只能用到自己归属下的 key。删除前需先迁走该归属下的用户和 key。
            </CardDescription>
          </div>
          <Button size="sm" onClick={openCreateGroup}>
            <Plus className="size-4" /> 新增归属
          </Button>
        </CardHeader>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead>名称</TableHead>
                <TableHead>备注</TableHead>
                <TableHead>用户数</TableHead>
                <TableHead>上游 Key 数</TableHead>
                <TableHead>启用</TableHead>
                <TableHead className="text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {groups.length === 0 && <TableEmpty colSpan={7} />}
              {groups.map((g) => (
                <TableRow key={g.id}>
                  <TableCell className="text-muted-foreground">{g.id}</TableCell>
                  <TableCell className="font-medium">
                    {g.name}
                    {String(g.id) === (s.default_group_id ?? '1') && (
                      <Badge variant="outline" className="ml-2">
                        默认
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-muted-foreground">{g.remark || '—'}</TableCell>
                  <TableCell className="tabular-nums">{g.user_count ?? 0}</TableCell>
                  <TableCell className="tabular-nums">{g.key_count ?? 0}</TableCell>
                  <TableCell>
                    <Switch
                      checked={g.enabled}
                      onCheckedChange={async (v) => {
                        await api.updateGroup(g.id, { enabled: v, name: g.name, remark: g.remark })
                        loadGroups()
                      }}
                    />
                  </TableCell>
                  <TableCell className="text-right">
                    <Button variant="ghost" size="icon" onClick={() => openEditGroup(g)}>
                      <Pencil className="size-4" />
                    </Button>
                    <Button variant="ghost" size="icon" onClick={() => removeGroup(g)}>
                      <Trash2 className="size-4 text-destructive" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <Dialog open={groupOpen} onOpenChange={setGroupOpen}>
        <DialogContent className="max-w-md">
          <DialogHeader>
            <DialogTitle>{editingGroup ? '编辑归属' : '新增归属'}</DialogTitle>
          </DialogHeader>
          <form onSubmit={saveGroup} className="space-y-4">
            <div className="space-y-1.5">
              <Label>名称</Label>
              <Input
                value={groupForm.name}
                onChange={(e) => setGroupForm({ ...groupForm, name: e.target.value })}
                placeholder="如 研发部 / 客服组"
                required
              />
            </div>
            <div className="space-y-1.5">
              <Label>备注</Label>
              <Input value={groupForm.remark} onChange={(e) => setGroupForm({ ...groupForm, remark: e.target.value })} />
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setGroupOpen(false)}>
                取消
              </Button>
              <Button type="submit">保存</Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </div>
  )
}
