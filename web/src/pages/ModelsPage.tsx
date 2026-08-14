import { useEffect, useState } from 'react'
import { toast } from 'sonner'
import { ChevronDown, ChevronRight, Link2, Pencil, Plus, Trash2 } from 'lucide-react'
import { api, type Binding, type Channel, type Model } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge, Label, Select, Switch } from '@/components/ui/misc'
import { Card, CardContent } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Table, TableBody, TableCell, TableEmpty, TableHead, TableHeader, TableRow } from '@/components/ui/table'

export default function ModelsPage() {
  const [models, setModels] = useState<Model[]>([])
  const [channels, setChannels] = useState<Channel[]>([])
  const [expanded, setExpanded] = useState<Set<number>>(new Set())

  const [modelOpen, setModelOpen] = useState(false)
  const [editingModel, setEditingModel] = useState<Model | null>(null)
  const [modelForm, setModelForm] = useState({ name: '', remark: '', enabled: true })

  const [bindOpen, setBindOpen] = useState(false)
  const [bindModel, setBindModel] = useState<Model | null>(null)
  const [editingBind, setEditingBind] = useState<Binding | null>(null)
  const [bindForm, setBindForm] = useState({ channel_id: 0, upstream_model: '', weight: 1, enabled: true })

  const load = () => api.models().then(setModels).catch((e) => toast.error((e as Error).message))
  useEffect(() => {
    load()
    api.channels().then(setChannels).catch(() => {})
  }, [])

  const toggleExpand = (id: number) =>
    setExpanded((s) => {
      const n = new Set(s)
      n.has(id) ? n.delete(id) : n.add(id)
      return n
    })

  // ---- 模型 ----
  const openCreateModel = () => {
    setEditingModel(null)
    setModelForm({ name: '', remark: '', enabled: true })
    setModelOpen(true)
  }
  const openEditModel = (m: Model) => {
    setEditingModel(m)
    setModelForm({ name: m.name, remark: m.remark, enabled: m.enabled })
    setModelOpen(true)
  }
  const saveModel = async (e: React.FormEvent) => {
    e.preventDefault()
    try {
      if (editingModel) await api.updateModel(editingModel.id, modelForm)
      else await api.createModel(modelForm)
      toast.success('已保存')
      setModelOpen(false)
      load()
    } catch (err) {
      toast.error((err as Error).message)
    }
  }
  const removeModel = async (m: Model) => {
    if (!confirm(`删除模型「${m.name}」及其所有绑定？`)) return
    try {
      await api.deleteModel(m.id)
      toast.success('已删除')
      load()
    } catch (err) {
      toast.error((err as Error).message)
    }
  }

  // ---- 绑定 ----
  const openCreateBind = (m: Model) => {
    if (channels.length === 0) {
      toast.error('请先录入上游')
      return
    }
    setBindModel(m)
    setEditingBind(null)
    setBindForm({ channel_id: channels[0].id, upstream_model: m.name, weight: 1, enabled: true })
    setBindOpen(true)
  }
  const openEditBind = (m: Model, b: Binding) => {
    setBindModel(m)
    setEditingBind(b)
    setBindForm({ channel_id: b.channel_id, upstream_model: b.upstream_model, weight: b.weight, enabled: b.enabled })
    setBindOpen(true)
  }
  const saveBind = async (e: React.FormEvent) => {
    e.preventDefault()
    try {
      if (editingBind) await api.updateBinding(editingBind.id, bindForm)
      else await api.createBinding(bindModel!.id, bindForm)
      toast.success('已保存')
      setBindOpen(false)
      setExpanded((s) => new Set(s).add(bindModel!.id))
      load()
    } catch (err) {
      toast.error((err as Error).message)
    }
  }
  const removeBind = async (b: Binding) => {
    if (!confirm('删除该绑定？')) return
    try {
      await api.deleteBinding(b.id)
      toast.success('已删除')
      load()
    } catch (err) {
      toast.error((err as Error).message)
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex items-end justify-between">
        <div>
          <h1 className="text-xl font-semibold">模型</h1>
          <p className="text-sm text-muted-foreground">对外模型名 + 绑定到上游（可多绑定，由路由策略选择）</p>
        </div>
        <Button onClick={openCreateModel}>
          <Plus className="size-4" /> 新增模型
        </Button>
      </div>

      <Card>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-10" />
                <TableHead>ID</TableHead>
                <TableHead>对外模型名</TableHead>
                <TableHead>备注</TableHead>
                <TableHead>绑定数</TableHead>
                <TableHead>启用</TableHead>
                <TableHead className="text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {models.length === 0 && <TableEmpty colSpan={7} />}
              {models.map((m) => {
                const open = expanded.has(m.id)
                const bindings = m.bindings ?? []
                return [
                  <TableRow key={m.id}>
                    <TableCell>
                      <button className="cursor-pointer text-muted-foreground" onClick={() => toggleExpand(m.id)}>
                        {open ? <ChevronDown className="size-4" /> : <ChevronRight className="size-4" />}
                      </button>
                    </TableCell>
                    <TableCell className="text-muted-foreground">{m.id}</TableCell>
                    <TableCell className="font-mono font-medium">{m.name}</TableCell>
                    <TableCell className="text-muted-foreground">{m.remark || '—'}</TableCell>
                    <TableCell>
                      <Badge variant={bindings.length ? 'secondary' : 'destructive'}>{bindings.length}</Badge>
                    </TableCell>
                    <TableCell>
                      <Switch
                        checked={m.enabled}
                        onCheckedChange={async (v) => {
                          await api.updateModel(m.id, { enabled: v, remark: m.remark })
                          load()
                        }}
                      />
                    </TableCell>
                    <TableCell className="whitespace-nowrap text-right">
                      <Button variant="ghost" size="sm" onClick={() => openCreateBind(m)}>
                        <Link2 className="size-4" /> 绑定
                      </Button>
                      <Button variant="ghost" size="icon" onClick={() => openEditModel(m)}>
                        <Pencil className="size-4" />
                      </Button>
                      <Button variant="ghost" size="icon" onClick={() => removeModel(m)}>
                        <Trash2 className="size-4 text-destructive" />
                      </Button>
                    </TableCell>
                  </TableRow>,
                  open && (
                    <TableRow key={`${m.id}-b`}>
                      <TableCell colSpan={7} className="bg-muted/40 p-4">
                        {bindings.length === 0 ? (
                          <p className="text-sm text-muted-foreground">还没有绑定，点右侧「绑定」添加上游。</p>
                        ) : (
                          <div className="space-y-2">
                            {bindings.map((b) => (
                              <div
                                key={b.id}
                                className="flex flex-wrap items-center gap-3 rounded-md border border-border bg-card px-3 py-2 text-sm"
                              >
                                <Badge variant="outline">#{b.id}</Badge>
                                <span className="font-medium">{b.channel?.name ?? `上游 ${b.channel_id}`}</span>
                                <span className="text-muted-foreground">→</span>
                                <code className="rounded bg-muted px-1.5 py-0.5 text-xs">{b.upstream_model}</code>
                                <span className="text-xs text-muted-foreground">权重 {b.weight}</span>
                                {!b.enabled && <Badge variant="destructive">已禁用</Badge>}
                                <div className="ml-auto flex items-center gap-1">
                                  <Switch
                                    checked={b.enabled}
                                    onCheckedChange={async (v) => {
                                      await api.updateBinding(b.id, { enabled: v })
                                      load()
                                    }}
                                  />
                                  <Button variant="ghost" size="icon" onClick={() => openEditBind(m, b)}>
                                    <Pencil className="size-4" />
                                  </Button>
                                  <Button variant="ghost" size="icon" onClick={() => removeBind(b)}>
                                    <Trash2 className="size-4 text-destructive" />
                                  </Button>
                                </div>
                              </div>
                            ))}
                          </div>
                        )}
                      </TableCell>
                    </TableRow>
                  ),
                ]
              })}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      {/* 模型弹窗 */}
      <Dialog open={modelOpen} onOpenChange={setModelOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{editingModel ? '编辑模型' : '新增模型'}</DialogTitle>
            <DialogDescription>这里填客户端请求里使用的 model 名称</DialogDescription>
          </DialogHeader>
          <form onSubmit={saveModel} className="space-y-4">
            <div className="space-y-1.5">
              <Label>对外模型名</Label>
              <Input
                value={modelForm.name}
                onChange={(e) => setModelForm({ ...modelForm, name: e.target.value })}
                placeholder="gpt-4o-mini"
                required
              />
            </div>
            <div className="space-y-1.5">
              <Label>备注</Label>
              <Input value={modelForm.remark} onChange={(e) => setModelForm({ ...modelForm, remark: e.target.value })} />
            </div>
            <div className="flex items-center gap-2">
              <Switch
                id="men"
                checked={modelForm.enabled}
                onCheckedChange={(v) => setModelForm({ ...modelForm, enabled: v })}
              />
              <Label htmlFor="men">启用</Label>
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setModelOpen(false)}>
                取消
              </Button>
              <Button type="submit">保存</Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {/* 绑定弹窗 */}
      <Dialog open={bindOpen} onOpenChange={setBindOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {editingBind ? '编辑绑定' : '新增绑定'} · {bindModel?.name}
            </DialogTitle>
            <DialogDescription>上游模型名是真正发给上游的 model 值</DialogDescription>
          </DialogHeader>
          <form onSubmit={saveBind} className="space-y-4">
            <div className="space-y-1.5">
              <Label>上游</Label>
              <Select
                value={bindForm.channel_id}
                onChange={(e) => setBindForm({ ...bindForm, channel_id: Number(e.target.value) })}
              >
                {channels.map((c) => (
                  <option key={c.id} value={c.id}>
                    {c.name} ({c.base_url})
                  </option>
                ))}
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label>上游模型名</Label>
              <Input
                value={bindForm.upstream_model}
                onChange={(e) => setBindForm({ ...bindForm, upstream_model: e.target.value })}
                placeholder="gpt-4o-mini-2024-07-18"
                required
              />
            </div>
            <div className="space-y-1.5">
              <Label>权重（weighted 策略用）</Label>
              <Input
                type="number"
                min={1}
                value={bindForm.weight}
                onChange={(e) => setBindForm({ ...bindForm, weight: Number(e.target.value) })}
              />
            </div>
            <div className="flex items-center gap-2">
              <Switch id="ben" checked={bindForm.enabled} onCheckedChange={(v) => setBindForm({ ...bindForm, enabled: v })} />
              <Label htmlFor="ben">启用</Label>
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setBindOpen(false)}>
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
