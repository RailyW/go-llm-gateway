import { useEffect, useState } from 'react'
import { toast } from 'sonner'
import { Pencil, Plus, Trash2 } from 'lucide-react'
import { api, type Channel, type ProtocolInfo } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge, Label, Switch } from '@/components/ui/misc'
import { Card, CardContent } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Table, TableBody, TableCell, TableEmpty, TableHead, TableHeader, TableRow } from '@/components/ui/table'

interface Form {
  name: string
  protocols: string[]
  base_url: string
  api_key: string
  enabled: boolean
}

export default function ChannelsPage() {
  const [list, setList] = useState<Channel[]>([])
  const [protocols, setProtocols] = useState<ProtocolInfo[]>([])
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<Channel | null>(null)
  const [form, setForm] = useState<Form>({ name: '', protocols: [], base_url: '', api_key: '', enabled: true })

  const load = () => api.channels().then(setList).catch((e) => toast.error((e as Error).message))
  useEffect(() => {
    load()
    api.meta().then((m) => setProtocols(m.protocols)).catch(() => {})
  }, [])

  const label = (name: string) => protocols.find((p) => p.name === name)?.label ?? name

  const openCreate = () => {
    setEditing(null)
    setForm({
      name: '',
      protocols: protocols.filter((p) => p.default).map((p) => p.name),
      base_url: '',
      api_key: '',
      enabled: true,
    })
    setOpen(true)
  }
  const openEdit = (c: Channel) => {
    setEditing(c)
    setForm({ name: c.name, protocols: [...c.protocols], base_url: c.base_url, api_key: '', enabled: c.enabled })
    setOpen(true)
  }

  const toggleProtocol = (name: string) =>
    setForm((f) => ({
      ...f,
      protocols: f.protocols.includes(name) ? f.protocols.filter((p) => p !== name) : [...f.protocols, name],
    }))

  const save = async (e: React.FormEvent) => {
    e.preventDefault()
    if (form.protocols.length === 0) {
      toast.error('至少勾选一个协议端点')
      return
    }
    try {
      if (editing) await api.updateChannel(editing.id, form)
      else await api.createChannel(form)
      toast.success('已保存')
      setOpen(false)
      load()
    } catch (err) {
      toast.error((err as Error).message)
    }
  }

  const remove = async (c: Channel) => {
    if (!confirm(`删除上游「${c.name}」？`)) return
    try {
      await api.deleteChannel(c.id)
      toast.success('已删除')
      load()
    } catch (err) {
      toast.error((err as Error).message)
    }
  }

  // 按厂商分组展示协议勾选框
  const vendors = [...new Set(protocols.map((p) => p.vendor))]

  return (
    <div className="space-y-4">
      <div className="flex items-end justify-between">
        <div>
          <h1 className="text-xl font-semibold">上游</h1>
          <p className="text-sm text-muted-foreground">录入 base_url / api_key，并勾选该上游**支持的端点协议**（同协议直转）</p>
        </div>
        <Button onClick={openCreate}>
          <Plus className="size-4" /> 新增上游
        </Button>
      </div>

      <Card>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead>名称</TableHead>
                <TableHead>支持的协议端点</TableHead>
                <TableHead>Base URL</TableHead>
                <TableHead>API Key</TableHead>
                <TableHead>启用</TableHead>
                <TableHead className="text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {list.length === 0 && <TableEmpty colSpan={7} />}
              {list.map((c) => (
                <TableRow key={c.id}>
                  <TableCell className="text-muted-foreground">{c.id}</TableCell>
                  <TableCell className="font-medium">{c.name}</TableCell>
                  <TableCell>
                    <div className="flex flex-wrap gap-1">
                      {c.protocols.map((p) => (
                        <Badge key={p} variant="outline" title={p}>
                          {label(p)}
                        </Badge>
                      ))}
                    </div>
                  </TableCell>
                  <TableCell className="max-w-60 truncate font-mono text-xs">{c.base_url}</TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">{c.api_key || '—'}</TableCell>
                  <TableCell>
                    <Switch
                      checked={c.enabled}
                      onCheckedChange={async (v) => {
                        await api.updateChannel(c.id, { enabled: v })
                        load()
                      }}
                    />
                  </TableCell>
                  <TableCell className="text-right">
                    <Button variant="ghost" size="icon" onClick={() => openEdit(c)}>
                      <Pencil className="size-4" />
                    </Button>
                    <Button variant="ghost" size="icon" onClick={() => remove(c)}>
                      <Trash2 className="size-4 text-destructive" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{editing ? '编辑上游' : '新增上游'}</DialogTitle>
            <DialogDescription>base_url 填到根或 /v1 都可以，例如 https://api.openai.com</DialogDescription>
          </DialogHeader>
          <form onSubmit={save} className="space-y-4">
            <div className="space-y-1.5">
              <Label>名称</Label>
              <Input value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} required />
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
                              checked={form.protocols.includes(p.name)}
                              onChange={() => toggleProtocol(p.name)}
                            />
                            <span>{p.label}</span>
                            <code className="rounded bg-muted px-1 text-xs text-muted-foreground">{p.path}</code>
                          </label>
                        ))}
                    </div>
                  </div>
                ))}
              </div>
              <p className="text-xs text-muted-foreground">
                客户端打哪个端点，就只会路由到勾选了该端点的上游；请求体与响应全程不做协议翻译。
              </p>
            </div>

            <div className="space-y-1.5">
              <Label>Base URL</Label>
              <Input
                value={form.base_url}
                onChange={(e) => setForm({ ...form, base_url: e.target.value })}
                placeholder="https://api.openai.com"
                required
              />
            </div>
            <div className="space-y-1.5">
              <Label>
                API Key {editing && <span className="text-xs text-muted-foreground">(留空表示不修改)</span>}
              </Label>
              <Input
                value={form.api_key}
                onChange={(e) => setForm({ ...form, api_key: e.target.value })}
                placeholder="sk-... / anthropic key"
                autoComplete="off"
              />
            </div>
            <div className="flex items-center gap-2">
              <Switch checked={form.enabled} onCheckedChange={(v) => setForm({ ...form, enabled: v })} id="en" />
              <Label htmlFor="en">启用</Label>
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setOpen(false)}>
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
