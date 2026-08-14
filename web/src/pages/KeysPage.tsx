import { useEffect, useState } from 'react'
import { toast } from 'sonner'
import { Copy, Plus, Trash2 } from 'lucide-react'
import { api, type ApiKey } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label, Switch } from '@/components/ui/misc'
import { Card, CardContent } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Table, TableBody, TableCell, TableEmpty, TableHead, TableHeader, TableRow } from '@/components/ui/table'

export default function KeysPage() {
  const [list, setList] = useState<ApiKey[]>([])
  const [open, setOpen] = useState(false)
  const [name, setName] = useState('default')

  const load = () => api.keys().then(setList).catch((e) => toast.error((e as Error).message))
  useEffect(() => {
    load()
  }, [])

  const create = async (e: React.FormEvent) => {
    e.preventDefault()
    try {
      const k = await api.createKey(name)
      toast.success('已创建，请复制保存')
      setOpen(false)
      setName('default')
      await navigator.clipboard.writeText(k.key).catch(() => {})
      load()
    } catch (err) {
      toast.error((err as Error).message)
    }
  }

  const copy = async (k: ApiKey) => {
    await navigator.clipboard.writeText(k.key)
    toast.success('已复制到剪贴板')
  }

  const remove = async (k: ApiKey) => {
    if (!confirm(`删除 key「${k.name}」？使用该 key 的请求将立即失败。`)) return
    await api.deleteKey(k.id)
    toast.success('已删除')
    load()
  }

  return (
    <div className="space-y-4">
      <div className="flex items-end justify-between">
        <div>
          <h1 className="text-xl font-semibold">API Key</h1>
          <p className="text-sm text-muted-foreground">本网关发放的 key，调用 /v1/chat/completions 时使用</p>
        </div>
        <Button onClick={() => setOpen(true)}>
          <Plus className="size-4" /> 新建 Key
        </Button>
      </div>

      <Card>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead>名称</TableHead>
                <TableHead>Key</TableHead>
                <TableHead>启用</TableHead>
                <TableHead>最近使用</TableHead>
                <TableHead>创建时间</TableHead>
                <TableHead className="text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {list.length === 0 && <TableEmpty colSpan={7} />}
              {list.map((k) => (
                <TableRow key={k.id}>
                  <TableCell className="text-muted-foreground">{k.id}</TableCell>
                  <TableCell className="font-medium">{k.name}</TableCell>
                  <TableCell>
                    <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs">{k.key}</code>
                  </TableCell>
                  <TableCell>
                    <Switch
                      checked={k.enabled}
                      onCheckedChange={async (v) => {
                        await api.updateKey(k.id, { enabled: v })
                        load()
                      }}
                    />
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {k.last_used_at ? new Date(k.last_used_at).toLocaleString() : '未使用'}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">{new Date(k.created_at).toLocaleString()}</TableCell>
                  <TableCell className="text-right">
                    <Button variant="ghost" size="icon" onClick={() => copy(k)}>
                      <Copy className="size-4" />
                    </Button>
                    <Button variant="ghost" size="icon" onClick={() => remove(k)}>
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
        <DialogContent className="max-w-md">
          <DialogHeader>
            <DialogTitle>新建 API Key</DialogTitle>
            <DialogDescription>创建后会自动复制到剪贴板</DialogDescription>
          </DialogHeader>
          <form onSubmit={create} className="space-y-4">
            <div className="space-y-1.5">
              <Label>名称</Label>
              <Input value={name} onChange={(e) => setName(e.target.value)} required />
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setOpen(false)}>
                取消
              </Button>
              <Button type="submit">创建</Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </div>
  )
}
