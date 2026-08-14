import { useEffect, useState } from 'react'
import { toast } from 'sonner'
import { ChevronDown, ChevronRight, KeyRound, Pencil, Plus, Trash2 } from 'lucide-react'
import { api, type Channel, type ChannelKey, type Group, type ProtocolInfo } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge, Label, Select, Switch } from '@/components/ui/misc'
import { Card, CardContent } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Table, TableBody, TableCell, TableEmpty, TableHead, TableHeader, TableRow } from '@/components/ui/table'

interface ChannelForm {
  name: string
  protocols: string[]
  base_url: string
  enabled: boolean
}
interface KeyForm {
  group_id: number
  name: string
  key: string
  weight: number
  enabled: boolean
}

export default function ChannelsPage() {
  const [list, setList] = useState<Channel[]>([])
  const [protocols, setProtocols] = useState<ProtocolInfo[]>([])
  const [groups, setGroups] = useState<Group[]>([])
  const [expanded, setExpanded] = useState<Set<number>>(new Set())

  const [chOpen, setChOpen] = useState(false)
  const [editingCh, setEditingCh] = useState<Channel | null>(null)
  const [chForm, setChForm] = useState<ChannelForm>({ name: '', protocols: [], base_url: '', enabled: true })

  const [keyOpen, setKeyOpen] = useState(false)
  const [keyChannel, setKeyChannel] = useState<Channel | null>(null)
  const [editingKey, setEditingKey] = useState<ChannelKey | null>(null)
  const [keyForm, setKeyForm] = useState<KeyForm>({ group_id: 0, name: '', key: '', weight: 1, enabled: true })

  const load = () => api.channels().then(setList).catch((e) => toast.error((e as Error).message))
  useEffect(() => {
    load()
    api.meta().then((m) => setProtocols(m.protocols)).catch(() => {})
    api.groups().then(setGroups).catch(() => {})
  }, [])

  const protoLabel = (name: string) => protocols.find((p) => p.name === name)?.label ?? name
  const groupName = (id: number) => groups.find((g) => g.id === id)?.name ?? `#${id}`

  const toggleExpand = (id: number) =>
    setExpanded((s) => {
      const n = new Set(s)
      n.has(id) ? n.delete(id) : n.add(id)
      return n
    })

  // ---------- 渠道 ----------
  const openCreateCh = () => {
    setEditingCh(null)
    setChForm({ name: '', protocols: protocols.filter((p) => p.default).map((p) => p.name), base_url: '', enabled: true })
    setChOpen(true)
  }
  const openEditCh = (c: Channel) => {
    setEditingCh(c)
    setChForm({ name: c.name, protocols: [...c.protocols], base_url: c.base_url, enabled: c.enabled })
    setChOpen(true)
  }
  const saveCh = async (e: React.FormEvent) => {
    e.preventDefault()
    if (chForm.protocols.length === 0) {
      toast.error('至少勾选一个协议端点')
      return
    }
    try {
      if (editingCh) await api.updateChannel(editingCh.id, chForm)
      else await api.createChannel(chForm)
      toast.success('已保存')
      setChOpen(false)
      load()
    } catch (err) {
      toast.error((err as Error).message)
    }
  }
  const removeCh = async (c: Channel) => {
    if (!confirm(`删除上游「${c.name}」及其所有 key？`)) return
    try {
      await api.deleteChannel(c.id)
      toast.success('已删除')
      load()
    } catch (err) {
      toast.error((err as Error).message)
    }
  }

  // ---------- 上游 key ----------
  const openCreateKey = (c: Channel) => {
    if (groups.length === 0) {
      toast.error('请先在设置页添加用户归属')
      return
    }
    setKeyChannel(c)
    setEditingKey(null)
    setKeyForm({ group_id: groups[0].id, name: '', key: '', weight: 1, enabled: true })
    setKeyOpen(true)
  }
  const openEditKey = (c: Channel, k: ChannelKey) => {
    setKeyChannel(c)
    setEditingKey(k)
    setKeyForm({ group_id: k.group_id, name: k.name, key: '', weight: k.weight, enabled: k.enabled })
    setKeyOpen(true)
  }
  const saveKey = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!editingKey && !keyForm.key.trim()) {
      toast.error('请填写上游 key')
      return
    }
    try {
      if (editingKey) await api.updateChannelKey(editingKey.id, keyForm)
      else await api.createChannelKey(keyChannel!.id, keyForm)
      toast.success('已保存')
      setKeyOpen(false)
      setExpanded((s) => new Set(s).add(keyChannel!.id))
      load()
    } catch (err) {
      toast.error((err as Error).message)
    }
  }
  const removeKey = async (k: ChannelKey) => {
    if (!confirm(`删除 key「${k.name}」？该归属的用户将无法再用这把 key。`)) return
    try {
      await api.deleteChannelKey(k.id)
      toast.success('已删除')
      load()
    } catch (err) {
      toast.error((err as Error).message)
    }
  }

  const vendors = [...new Set(protocols.map((p) => p.vendor))]

  return (
    <div className="space-y-4">
      <div className="flex items-end justify-between">
        <div>
          <h1 className="text-xl font-semibold">上游</h1>
          <p className="text-sm text-muted-foreground">
            一个上游可以有多把 key，每把 key 属于一个「用户归属」；用户只会用到自己归属下的 key
          </p>
        </div>
        <Button onClick={openCreateCh}>
          <Plus className="size-4" /> 新增上游
        </Button>
      </div>

      <Card>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-10" />
                <TableHead>ID</TableHead>
                <TableHead>名称</TableHead>
                <TableHead>支持的协议端点</TableHead>
                <TableHead>Base URL</TableHead>
                <TableHead>Key / 归属覆盖</TableHead>
                <TableHead>启用</TableHead>
                <TableHead className="text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {list.length === 0 && <TableEmpty colSpan={8} />}
              {list.map((c) => {
                const open = expanded.has(c.id)
                const keys = c.keys ?? []
                const coveredGroups = [...new Set(keys.filter((k) => k.enabled).map((k) => k.group_id))]
                return [
                  <TableRow key={c.id}>
                    <TableCell>
                      <button className="cursor-pointer text-muted-foreground" onClick={() => toggleExpand(c.id)}>
                        {open ? <ChevronDown className="size-4" /> : <ChevronRight className="size-4" />}
                      </button>
                    </TableCell>
                    <TableCell className="text-muted-foreground">{c.id}</TableCell>
                    <TableCell className="font-medium">{c.name}</TableCell>
                    <TableCell>
                      <div className="flex flex-wrap gap-1">
                        {c.protocols.map((p) => (
                          <Badge key={p} variant="outline" title={p}>
                            {protoLabel(p)}
                          </Badge>
                        ))}
                      </div>
                    </TableCell>
                    <TableCell className="max-w-52 truncate font-mono text-xs">{c.base_url}</TableCell>
                    <TableCell>
                      {keys.length === 0 ? (
                        <Badge variant="destructive">无 key</Badge>
                      ) : (
                        <div className="flex flex-wrap items-center gap-1">
                          <span className="text-xs text-muted-foreground">{keys.length} 把 /</span>
                          {coveredGroups.map((gid) => (
                            <Badge key={gid} variant="secondary">
                              {groupName(gid)}
                            </Badge>
                          ))}
                        </div>
                      )}
                    </TableCell>
                    <TableCell>
                      <Switch
                        checked={c.enabled}
                        onCheckedChange={async (v) => {
                          await api.updateChannel(c.id, { enabled: v })
                          load()
                        }}
                      />
                    </TableCell>
                    <TableCell className="whitespace-nowrap text-right">
                      <Button variant="ghost" size="sm" onClick={() => openCreateKey(c)}>
                        <KeyRound className="size-4" /> 加 Key
                      </Button>
                      <Button variant="ghost" size="icon" onClick={() => openEditCh(c)}>
                        <Pencil className="size-4" />
                      </Button>
                      <Button variant="ghost" size="icon" onClick={() => removeCh(c)}>
                        <Trash2 className="size-4 text-destructive" />
                      </Button>
                    </TableCell>
                  </TableRow>,
                  open && (
                    <TableRow key={`${c.id}-keys`}>
                      <TableCell colSpan={8} className="bg-muted/40 p-4">
                        {keys.length === 0 ? (
                          <p className="text-sm text-muted-foreground">
                            还没有 key。点右侧「加 Key」，并选择这把 key 给哪个归属使用。
                          </p>
                        ) : (
                          <div className="space-y-2">
                            {keys.map((k) => (
                              <div
                                key={k.id}
                                className="flex flex-wrap items-center gap-3 rounded-md border border-border bg-card px-3 py-2 text-sm"
                              >
                                <Badge variant="outline">#{k.id}</Badge>
                                <Badge variant="secondary">{k.group?.name ?? groupName(k.group_id)}</Badge>
                                <span className="font-medium">{k.name}</span>
                                <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs">{k.key_masked}</code>
                                <span className="text-xs text-muted-foreground">权重 {k.weight}</span>
                                <span className="text-xs text-muted-foreground">
                                  {k.last_used_at ? `最近使用 ${new Date(k.last_used_at).toLocaleString()}` : '未使用'}
                                </span>
                                {!k.enabled && <Badge variant="destructive">已禁用</Badge>}
                                <div className="ml-auto flex items-center gap-1">
                                  <Switch
                                    checked={k.enabled}
                                    onCheckedChange={async (v) => {
                                      await api.updateChannelKey(k.id, { enabled: v })
                                      load()
                                    }}
                                  />
                                  <Button variant="ghost" size="icon" onClick={() => openEditKey(c, k)}>
                                    <Pencil className="size-4" />
                                  </Button>
                                  <Button variant="ghost" size="icon" onClick={() => removeKey(k)}>
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

      {/* 渠道弹窗 */}
      <Dialog open={chOpen} onOpenChange={setChOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{editingCh ? '编辑上游' : '新增上游'}</DialogTitle>
            <DialogDescription>base_url 填到根或 /v1 都可以；key 在列表里按归属单独添加</DialogDescription>
          </DialogHeader>
          <form onSubmit={saveCh} className="space-y-4">
            <div className="space-y-1.5">
              <Label>名称</Label>
              <Input value={chForm.name} onChange={(e) => setChForm({ ...chForm, name: e.target.value })} required />
            </div>
            <div className="space-y-2">
              <Label>支持的协议端点</Label>
              <div className="space-y-3 rounded-md border border-border p-3">
                {vendors.map((v) => (
                  <div key={v}>
                    <div className="mb-1.5 text-xs font-medium uppercase text-muted-foreground">{v}</div>
                    <div className="space-y-1.5">
                      {protocols
                        .filter((p) => p.vendor === v)
                        .map((p) => (
                          <label key={p.name} className="flex cursor-pointer items-center gap-2 text-sm">
                            <input
                              type="checkbox"
                              className="size-4 cursor-pointer accent-[var(--primary)]"
                              checked={chForm.protocols.includes(p.name)}
                              onChange={() =>
                                setChForm((f) => ({
                                  ...f,
                                  protocols: f.protocols.includes(p.name)
                                    ? f.protocols.filter((x) => x !== p.name)
                                    : [...f.protocols, p.name],
                                }))
                              }
                            />
                            <span>{p.label}</span>
                            <code className="rounded bg-muted px-1 text-xs text-muted-foreground">{p.path}</code>
                          </label>
                        ))}
                    </div>
                  </div>
                ))}
              </div>
            </div>
            <div className="space-y-1.5">
              <Label>Base URL</Label>
              <Input
                value={chForm.base_url}
                onChange={(e) => setChForm({ ...chForm, base_url: e.target.value })}
                placeholder="https://api.openai.com"
                required
              />
            </div>
            <div className="flex items-center gap-2">
              <Switch checked={chForm.enabled} onCheckedChange={(v) => setChForm({ ...chForm, enabled: v })} id="cen" />
              <Label htmlFor="cen">启用</Label>
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setChOpen(false)}>
                取消
              </Button>
              <Button type="submit">保存</Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {/* 上游 key 弹窗 */}
      <Dialog open={keyOpen} onOpenChange={setKeyOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {editingKey ? '编辑上游 Key' : '新增上游 Key'} · {keyChannel?.name}
            </DialogTitle>
            <DialogDescription>该归属下的用户调用时会用这把 key；同一归属可以配多把（按策略选一把）</DialogDescription>
          </DialogHeader>
          <form onSubmit={saveKey} className="space-y-4">
            <div className="space-y-1.5">
              <Label>归属</Label>
              <Select
                value={keyForm.group_id}
                onChange={(e) => setKeyForm({ ...keyForm, group_id: Number(e.target.value) })}
              >
                {groups.map((g) => (
                  <option key={g.id} value={g.id}>
                    {g.name}
                    {g.remark ? ` (${g.remark})` : ''}
                  </option>
                ))}
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label>备注名</Label>
              <Input
                value={keyForm.name}
                onChange={(e) => setKeyForm({ ...keyForm, name: e.target.value })}
                placeholder="留空自动生成"
              />
            </div>
            <div className="space-y-1.5">
              <Label>
                上游 Key {editingKey && <span className="text-xs text-muted-foreground">(留空表示不修改)</span>}
              </Label>
              <Input
                value={keyForm.key}
                onChange={(e) => setKeyForm({ ...keyForm, key: e.target.value })}
                placeholder="sk-... / anthropic key"
                autoComplete="off"
              />
            </div>
            <div className="space-y-1.5">
              <Label>权重（weighted 策略用）</Label>
              <Input
                type="number"
                min={1}
                value={keyForm.weight}
                onChange={(e) => setKeyForm({ ...keyForm, weight: Number(e.target.value) })}
              />
            </div>
            <div className="flex items-center gap-2">
              <Switch checked={keyForm.enabled} onCheckedChange={(v) => setKeyForm({ ...keyForm, enabled: v })} id="ken" />
              <Label htmlFor="ken">启用</Label>
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setKeyOpen(false)}>
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
