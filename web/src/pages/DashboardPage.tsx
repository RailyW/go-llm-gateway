import { useEffect, useState } from 'react'
import { toast } from 'sonner'
import { api, type CleanerStatus, type ProtocolInfo } from '@/lib/api'
import { useAuth } from '@/lib/auth'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge, Label } from '@/components/ui/misc'

type Stats = Record<string, number | CleanerStatus>

export default function DashboardPage() {
  const { user } = useAuth()
  const [stats, setStats] = useState<Stats>({})
  const [protocols, setProtocols] = useState<ProtocolInfo[]>([])
  const [oldPw, setOldPw] = useState('')
  const [newPw, setNewPw] = useState('')

  useEffect(() => {
    api.stats().then(setStats).catch((e) => toast.error((e as Error).message))
    api.meta().then((m) => setProtocols(m.protocols)).catch(() => {})
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
            <CardDescription>同协议直转：打哪个端点就转到上游同名端点，不做协议翻译</CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="space-y-1.5">
              {protocols.map((p) => (
                <div key={p.name} className="flex items-center gap-2 text-xs">
                  <Badge variant="outline">{p.vendor}</Badge>
                  <code className="rounded bg-muted px-1.5 py-0.5">POST {base}{p.path}</code>
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
              <Button type="submit" size="sm">保存</Button>
            </form>
          </CardContent>
        </Card>
      </div>
    </div>
  )
}
