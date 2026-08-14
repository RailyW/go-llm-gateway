import { useEffect, useState } from 'react'
import { toast } from 'sonner'
import { api, type CleanerStatus } from '@/lib/api'
import { useAuth } from '@/lib/auth'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/misc'

type Stats = Record<string, number | CleanerStatus>

export default function DashboardPage() {
  const { user } = useAuth()
  const [stats, setStats] = useState<Stats>({})
  const [oldPw, setOldPw] = useState('')
  const [newPw, setNewPw] = useState('')

  useEffect(() => {
    api.stats().then(setStats).catch((e) => toast.error((e as Error).message))
  }, [])

  const n = (k: string) => (typeof stats[k] === 'number' ? (stats[k] as number) : 0)
  const cards = [
    { label: '请求总数', value: n('requests') },
    { label: '失败请求', value: n('errors') },
    { label: 'Prompt tokens', value: n('prompt_tokens') },
    { label: 'Completion tokens', value: n('completion_tokens') },
    { label: '我的 Key', value: n('keys') },
    ...(user?.role === 'admin'
      ? [
          { label: '上游数', value: n('channels') },
          { label: '模型数', value: n('models') },
          { label: '用户数', value: n('users') },
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

  const base = location.origin
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-xl font-semibold">概览</h1>
        <p className="text-sm text-muted-foreground">欢迎回来，{user?.username}</p>
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

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>调用方式</CardTitle>
          </CardHeader>
          <CardContent>
            <pre className="overflow-x-auto rounded-md bg-muted p-3 text-xs leading-relaxed">
{`curl ${base}/v1/chat/completions \\
  -H "Authorization: Bearer sk-你的网关key" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "你录入的模型名",
    "stream": false,
    "messages": [{"role":"user","content":"hello"}]
  }'`}
            </pre>
            <p className="mt-3 text-xs text-muted-foreground">
              Base URL: <code className="rounded bg-muted px-1">{base}/v1</code> ·
              仅支持 <code className="rounded bg-muted px-1">/chat/completions</code> 与 <code className="rounded bg-muted px-1">/models</code>
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
              <Button type="submit" size="sm">保存</Button>
            </form>
          </CardContent>
        </Card>
      </div>
    </div>
  )
}
