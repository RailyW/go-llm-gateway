import { useEffect, useState } from 'react'
import { toast } from 'sonner'
import { Pencil, Plus, Trash2 } from 'lucide-react'
import { api, type Channel } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge, Label, Select, Switch } from '@/components/ui/misc'
import { Card, CardContent } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Table, TableBody, TableCell, TableEmpty, TableHead, TableHeader, TableRow } from '@/components/ui/table'

interface Form {
  name: string
  type: string
  base_url: string
  api_key: string
  enabled: boolean
}
const empty: Form = { name: '', type: 'openai', base_url: '', api_key: '', enabled: true }

export default function ChannelsPage() {
  const [list, setList] = useState<Channel[]>([])
  const [types, setTypes] = useState<string[]>(['openai'])
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<Channel | null>(null)
  const [form, setForm] = useState<Form>(empty)

  const load = () => api.channels().then(setList).catch((e) => toast.error((e as Error).message))
  useEffect(() => {
    load()
    api.meta().then((m) => setTypes(m.channel_types)).catch(() => {})
  }, [])

  const openCreate = () => {
    setEditing(null)
    setForm(empty)
    setOpen(true)
  }
  const openEdit = (c: Channel) => {
    setEditing(c)
    setForm({ name: c.name, type: c.type, base_url: c.base_url, api_key: '', enabled: c.enabled })
    setOpen(true)
  }

  const save = async (e: React.FormEvent) => {
    e.preventDefault()
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

  const toggle = async (c: Channel, enabled: boolean) => {
    try {
      await api.updateChannel(c.id, { enabled })
      load()
    } catch (err) {
      toast.error((err as Error).message)
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex items-end justify-between">
        <div>
          <h1 className="text-xl font-semibold">上游</h1>
          <p className="text-sm text-muted-foreground">录入上游的 base_url 与 api_key（OpenAI 兼容）</p>
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
                <TableHead>协议</TableHead>
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
                    <Badge variant="outline">{c.type}</Badge>
                  </TableCell>
                  <TableCell className="max-w-70 truncate font-mono text-xs">{c.base_url}</TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">{c.api_key || '—'}</TableCell>
                  <TableCell>
                    <Switch checked={c.enabled} onCheckedChange={(v) => toggle(c, v)} />
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
            <div className="space-y-1.5">
              <Label>协议类型</Label>
              <Select value={form.type} onChange={(e) => setForm({ ...form, type: e.target.value })}>
                {types.map((t) => (
                  <option key={t} value={t}>
                    {t}
                  </option>
                ))}
              </Select>
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
              <Label>API Key {editing && <span className="text-xs text-muted-foreground">(留空表示不修改)</span>}</Label>
              <Input
                value={form.api_key}
                onChange={(e) => setForm({ ...form, api_key: e.target.value })}
                placeholder="sk-..."
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
