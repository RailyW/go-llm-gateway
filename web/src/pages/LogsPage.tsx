import { useCallback, useEffect, useState } from 'react'
import { toast } from 'sonner'
import { FileText, RefreshCw, Search } from 'lucide-react'
import { api, type LogItem } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge, Select } from '@/components/ui/misc'
import { Card, CardContent } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Table, TableBody, TableCell, TableEmpty, TableHead, TableHeader, TableRow } from '@/components/ui/table'

export default function LogsPage() {
  const [items, setItems] = useState<LogItem[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [pageSize] = useState(20)
  const [keyword, setKeyword] = useState('')
  const [status, setStatus] = useState('')
  const [loading, setLoading] = useState(false)

  const [detail, setDetail] = useState<LogItem | null>(null)
  const [archive, setArchive] = useState<{ request: string; response: string } | null>(null)
  const [archiveErr, setArchiveErr] = useState('')

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const res = await api.logs({ page, page_size: pageSize, keyword, status })
      setItems(res.items)
      setTotal(res.total)
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setLoading(false)
    }
  }, [page, pageSize, keyword, status])

  useEffect(() => {
    load()
  }, [load])

  const openDetail = async (l: LogItem) => {
    setDetail(l)
    setArchive(null)
    setArchiveErr('')
    try {
      setArchive(await api.logArchive(l.id))
    } catch (e) {
      setArchiveErr((e as Error).message)
    }
  }

  const pages = Math.max(1, Math.ceil(total / pageSize))

  return (
    <div className="space-y-4">
      <div className="flex items-end justify-between">
        <div>
          <h1 className="text-xl font-semibold">日志</h1>
          <p className="text-sm text-muted-foreground">共 {total} 条 · 点「原文」查看本地归档的请求/响应全文</p>
        </div>
        <Button variant="outline" onClick={load} disabled={loading}>
          <RefreshCw className={loading ? 'size-4 animate-spin' : 'size-4'} /> 刷新
        </Button>
      </div>

      <div className="flex flex-wrap gap-2">
        <div className="relative">
          <Search className="pointer-events-none absolute left-2.5 top-2.5 size-4 text-muted-foreground" />
          <Input
            className="w-64 pl-8"
            placeholder="搜索 request id / 用户 / 模型 / 上游"
            value={keyword}
            onChange={(e) => {
              setPage(1)
              setKeyword(e.target.value)
            }}
          />
        </div>
        <Select
          className="w-32"
          value={status}
          onChange={(e) => {
            setPage(1)
            setStatus(e.target.value)
          }}
        >
          <option value="">全部状态</option>
          <option value="ok">成功</option>
          <option value="error">失败</option>
        </Select>
      </div>

      <Card>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>时间</TableHead>
                <TableHead>用户</TableHead>
                <TableHead>端点</TableHead>
                <TableHead>模型</TableHead>
                <TableHead>上游</TableHead>
                <TableHead>状态</TableHead>
                <TableHead>Tokens</TableHead>
                <TableHead>耗时</TableHead>
                <TableHead className="text-right">原文</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {items.length === 0 && <TableEmpty colSpan={9} />}
              {items.map((l) => (
                <TableRow key={l.id}>
                  <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                    {new Date(l.created_at).toLocaleString()}
                  </TableCell>
                  <TableCell>{l.username}</TableCell>
                  <TableCell className="whitespace-nowrap font-mono text-xs" title={l.protocol}>
                    {l.endpoint || '—'}
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {l.model_name}
                    {l.stream && <Badge variant="outline" className="ml-1">stream</Badge>}
                  </TableCell>
                  <TableCell className="text-xs">
                    {l.channel_name ? (
                      <>
                        {l.channel_name} <span className="text-muted-foreground">/ {l.upstream_model}</span>
                      </>
                    ) : (
                      '—'
                    )}
                  </TableCell>
                  <TableCell>
                    <Badge variant={l.status_code < 400 ? 'success' : 'destructive'}>{l.status_code}</Badge>
                  </TableCell>
                  <TableCell className="whitespace-nowrap text-xs tabular-nums">
                    {l.prompt_tokens}/{l.completion_tokens}
                  </TableCell>
                  <TableCell className="text-xs tabular-nums">{l.duration_ms}ms</TableCell>
                  <TableCell className="text-right">
                    <Button variant="ghost" size="icon" onClick={() => openDetail(l)}>
                      <FileText className="size-4" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <div className="flex items-center justify-end gap-2 text-sm">
        <span className="text-muted-foreground">
          第 {page} / {pages} 页
        </span>
        <Button variant="outline" size="sm" disabled={page <= 1} onClick={() => setPage((p) => p - 1)}>
          上一页
        </Button>
        <Button variant="outline" size="sm" disabled={page >= pages} onClick={() => setPage((p) => p + 1)}>
          下一页
        </Button>
      </div>

      <Dialog open={!!detail} onOpenChange={(o) => !o && setDetail(null)}>
        <DialogContent className="max-w-[min(1000px,92vw)]">
          <DialogHeader>
            <DialogTitle>请求原文</DialogTitle>
            <DialogDescription className="font-mono text-xs">
              {detail?.id} · {detail?.endpoint} · {detail?.model_name} → {detail?.channel_name}/{detail?.upstream_model}
            </DialogDescription>
          </DialogHeader>
          {detail?.error_message && (
            <div className="mb-3 rounded-md border border-destructive/40 bg-destructive/10 p-3 text-xs">
              {detail.error_message}
            </div>
          )}
          {archiveErr && <p className="text-sm text-muted-foreground">{archiveErr}</p>}
          {archive && (
            <div className="grid gap-3 lg:grid-cols-2">
              <div>
                <div className="mb-1 text-xs font-medium text-muted-foreground">请求</div>
                <pre className="max-h-[55vh] overflow-auto rounded-md bg-muted p-3 text-xs whitespace-pre-wrap break-all">
                  {archive.request}
                </pre>
              </div>
              <div>
                <div className="mb-1 text-xs font-medium text-muted-foreground">响应</div>
                <pre className="max-h-[55vh] overflow-auto rounded-md bg-muted p-3 text-xs whitespace-pre-wrap break-all">
                  {archive.response}
                </pre>
              </div>
            </div>
          )}
        </DialogContent>
      </Dialog>
    </div>
  )
}
