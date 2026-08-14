import { useEffect, useState } from 'react'
import { toast } from 'sonner'
import { KeyRound, Trash2 } from 'lucide-react'
import { api, type Group, type User } from '@/lib/api'
import { useAuth } from '@/lib/auth'
import { Button } from '@/components/ui/button'
import { Badge, Select, Switch } from '@/components/ui/misc'
import { Card, CardContent } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableEmpty, TableHead, TableHeader, TableRow } from '@/components/ui/table'

export default function UsersPage() {
  const { user: me } = useAuth()
  const [list, setList] = useState<User[]>([])
  const [groups, setGroups] = useState<Group[]>([])

  const load = () => api.users().then(setList).catch((e) => toast.error((e as Error).message))
  useEffect(() => {
    load()
    api.groups().then(setGroups).catch(() => {})
  }, [])

  const update = async (u: User, v: Parameters<typeof api.updateUser>[1]) => {
    try {
      await api.updateUser(u.id, v)
      toast.success('已更新')
      load()
    } catch (e) {
      toast.error((e as Error).message)
    }
  }

  const resetPw = async (u: User) => {
    const pw = prompt(`为「${u.username}」设置新密码（至少 4 位）`)
    if (!pw) return
    await update(u, { password: pw })
  }

  const remove = async (u: User) => {
    if (!confirm(`删除用户「${u.username}」及其所有 key？`)) return
    try {
      await api.deleteUser(u.id)
      toast.success('已删除')
      load()
    } catch (e) {
      toast.error((e as Error).message)
    }
  }

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-xl font-semibold">用户</h1>
        <p className="text-sm text-muted-foreground">
          角色、归属、启停与密码重置。归属决定该用户能用哪些上游 key（归属枚举在设置页维护）。
        </p>
      </div>

      <Card>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead>用户名</TableHead>
                <TableHead>角色</TableHead>
                <TableHead>归属</TableHead>
                <TableHead>启用</TableHead>
                <TableHead>注册时间</TableHead>
                <TableHead className="text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {list.length === 0 && <TableEmpty colSpan={7} />}
              {list.map((u) => (
                <TableRow key={u.id}>
                  <TableCell className="text-muted-foreground">{u.id}</TableCell>
                  <TableCell className="font-medium">
                    {u.username}
                    {u.id === me?.id && <Badge variant="outline" className="ml-2">我</Badge>}
                  </TableCell>
                  <TableCell>
                    <Select className="w-28" value={u.role} onChange={(e) => update(u, { role: e.target.value })}>
                      <option value="admin">admin</option>
                      <option value="user">user</option>
                    </Select>
                  </TableCell>
                  <TableCell>
                    <Select className="w-40" value={u.group_id} onChange={(e) => update(u, { group_id: Number(e.target.value) })}>
                      {groups.map((g) => (
                        <option key={g.id} value={g.id}>
                          {g.name}
                        </option>
                      ))}
                      {!groups.some((g) => g.id === u.group_id) && <option value={u.group_id}>#{u.group_id}</option>}
                    </Select>
                  </TableCell>
                  <TableCell>
                    <Switch checked={u.enabled} disabled={u.id === me?.id} onCheckedChange={(v) => update(u, { enabled: v })} />
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">{new Date(u.created_at).toLocaleString()}</TableCell>
                  <TableCell className="text-right">
                    <Button variant="ghost" size="icon" onClick={() => resetPw(u)} title="重置密码">
                      <KeyRound className="size-4" />
                    </Button>
                    <Button variant="ghost" size="icon" disabled={u.id === me?.id} onClick={() => remove(u)}>
                      <Trash2 className="size-4 text-destructive" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  )
}
