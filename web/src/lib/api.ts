const TOKEN_KEY = 'gw_token'

export const getToken = () => localStorage.getItem(TOKEN_KEY)
export const setToken = (t: string) => localStorage.setItem(TOKEN_KEY, t)
export const clearToken = () => localStorage.removeItem(TOKEN_KEY)

export interface Group {
  id: number
  name: string
  remark: string
  enabled: boolean
  created_at: string
  user_count?: number
  key_count?: number
}
export interface User {
  id: number
  username: string
  role: 'admin' | 'user'
  group_id: number
  group?: Group
  enabled: boolean
  created_at: string
}
export interface ChannelKey {
  id: number
  channel_id: number
  group_id: number
  group?: Group
  name: string
  key_masked: string
  weight: number
  enabled: boolean
  last_used_at: string | null
  created_at: string
}
export interface ProtocolInfo {
  name: string
  label: string
  vendor: string
  path: string
  default: boolean
}
export interface Channel {
  id: number
  name: string
  protocols: string[]
  base_url: string
  enabled: boolean
  created_at: string
  keys?: ChannelKey[]
}
export interface Binding {
  id: number
  model_id: number
  channel_id: number
  upstream_model: string
  weight: number
  enabled: boolean
  channel?: Channel
}
export interface Model {
  id: number
  name: string
  remark: string
  enabled: boolean
  bindings?: Binding[]
}
export interface ApiKey {
  id: number
  user_id: number
  name: string
  key: string
  enabled: boolean
  last_used_at: string | null
  created_at: string
}
export interface LogItem {
  id: string
  protocol: string
  endpoint: string
  group_name: string
  channel_key_name: string
  username: string
  api_key_name: string
  model_name: string
  channel_name: string
  upstream_model: string
  stream: boolean
  status_code: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  duration_ms: number
  client_ip: string
  error_message: string
  archive_path: string
  created_at: string
}
export interface SinkStats {
  enqueued: number
  dropped: number
  persisted: number
  batches: number
  queue_len: number
  queue_cap: number
  last_flush_at: string
  last_flush_ms: number
  last_batch_len: number
  last_error: string
  using_copy: boolean
  active: boolean
}
export interface RegistryStats {
  built_at: string
  reloads: number
  callers: number
  models: number
  key_sets: number
}
export interface RedisStats {
  enabled: boolean
  healthy: boolean
  breaker_open: boolean
  calls: number
  failures: number
  degradations: number
  degraded_since?: string
  last_error?: string
  addr?: string
}

export interface InstanceInfo {
  id: string
  role: string
  role_label: string
  archive_usable: boolean
}

export interface Peer {
  instance: string
  role: string
  at: string
  port?: string
  persists?: boolean
  logs_dropped?: number
  sink?: SinkStats
  registry?: RegistryStats
}

export interface Stats {
  window: '1h' | '24h'
  since: string
  requests: number
  errors: number
  prompt_tokens: number
  completion_tokens: number
  keys: number
  channels?: number
  models?: number
  users?: number
  groups?: number
  cleaner?: CleanerStatus
  sink?: SinkStats
  registry?: RegistryStats
  instance?: InstanceInfo
  redis?: RedisStats
  invalidate?: Record<string, unknown>
  cluster?: Peer[]
}
export interface CleanerStatus {
  last_run_at: string | null
  next_run_at: string | null
  last_removed_archive_dirs: number
  last_removed_log_rows: number
  last_error: string
  using_copy: boolean
  active: boolean
  running: boolean
}

class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  if (init.body) headers.set('Content-Type', 'application/json')
  const token = getToken()
  if (token) headers.set('Authorization', `Bearer ${token}`)

  const res = await fetch(`/api${path}`, { ...init, headers })
  const text = await res.text()
  const data = text ? JSON.parse(text) : {}
  if (!res.ok) {
    if (res.status === 401) {
      clearToken()
      if (!location.pathname.startsWith('/login')) location.href = '/login'
    }
    throw new ApiError(res.status, data.error || `请求失败 (${res.status})`)
  }
  return data as T
}

const body = (v: unknown) => JSON.stringify(v)

export const api = {
  meta: () => request<{ allow_register: boolean; protocols: ProtocolInfo[]; strategies: string[] }>('/meta'),
  login: (username: string, password: string) =>
    request<{ token: string; user: User }>('/auth/login', { method: 'POST', body: body({ username, password }) }),
  register: (username: string, password: string) =>
    request<{ token: string; user: User }>('/auth/register', { method: 'POST', body: body({ username, password }) }),
  me: () => request<User>('/auth/me'),
  changePassword: (old_password: string, new_password: string) =>
    request<{ ok: boolean }>('/auth/password', { method: 'POST', body: body({ old_password, new_password }) }),

  stats: (window: '1h' | '24h' = '1h') => request<Stats>(`/stats?window=${window}`),

  channels: () => request<Channel[]>('/channels'),
  channelKeys: (channelId: number) => request<ChannelKey[]>(`/channels/${channelId}/keys`),
  createChannelKey: (channelId: number, v: { group_id: number; name?: string; key: string; weight?: number }) =>
    request<ChannelKey>(`/channels/${channelId}/keys`, { method: 'POST', body: body(v) }),
  updateChannelKey: (id: number, v: Partial<{ group_id: number; name: string; key: string; weight: number; enabled: boolean }>) =>
    request<ChannelKey>(`/channel-keys/${id}`, { method: 'PUT', body: body(v) }),
  deleteChannelKey: (id: number) => request<void>(`/channel-keys/${id}`, { method: 'DELETE' }),

  groups: () => request<Group[]>('/groups'),
  createGroup: (v: { name: string; remark?: string }) => request<Group>('/groups', { method: 'POST', body: body(v) }),
  updateGroup: (id: number, v: Partial<{ name: string; remark: string; enabled: boolean }>) =>
    request<Group>(`/groups/${id}`, { method: 'PUT', body: body(v) }),
  deleteGroup: (id: number) => request<void>(`/groups/${id}`, { method: 'DELETE' }),
  createChannel: (v: Partial<Channel>) => request<Channel>('/channels', { method: 'POST', body: body(v) }),
  updateChannel: (id: number, v: Partial<Channel>) => request<Channel>(`/channels/${id}`, { method: 'PUT', body: body(v) }),
  deleteChannel: (id: number) => request<void>(`/channels/${id}`, { method: 'DELETE' }),

  models: () => request<Model[]>('/models'),
  createModel: (v: Partial<Model>) => request<Model>('/models', { method: 'POST', body: body(v) }),
  updateModel: (id: number, v: Partial<Model>) => request<Model>(`/models/${id}`, { method: 'PUT', body: body(v) }),
  deleteModel: (id: number) => request<void>(`/models/${id}`, { method: 'DELETE' }),

  createBinding: (modelId: number, v: Partial<Binding>) =>
    request<Binding>(`/models/${modelId}/bindings`, { method: 'POST', body: body(v) }),
  updateBinding: (id: number, v: Partial<Binding>) => request<Binding>(`/bindings/${id}`, { method: 'PUT', body: body(v) }),
  deleteBinding: (id: number) => request<void>(`/bindings/${id}`, { method: 'DELETE' }),

  keys: (all = false) => request<ApiKey[]>(`/keys${all ? '?all=1' : ''}`),
  createKey: (name: string) => request<ApiKey>('/keys', { method: 'POST', body: body({ name }) }),
  updateKey: (id: number, v: { name?: string; enabled?: boolean }) =>
    request<ApiKey>(`/keys/${id}`, { method: 'PUT', body: body(v) }),
  deleteKey: (id: number) => request<void>(`/keys/${id}`, { method: 'DELETE' }),

  logs: (params: Record<string, string | number>) => {
    const q = new URLSearchParams(Object.entries(params).map(([k, v]) => [k, String(v)])).toString()
    return request<{ total: number; page: number; page_size: number; items: LogItem[] }>(`/logs?${q}`)
  },
  logArchive: (id: string) => request<{ request: string; response: string }>(`/logs/${id}/archive`),

  settings: () =>
    request<{
      settings: Record<string, string>
      strategies: string[]
      key_strategies: string[]
      protocols: ProtocolInfo[]
      cleaner: CleanerStatus
    }>('/settings'),
  updateSettings: (v: Record<string, string>) =>
    request<{ settings: Record<string, string> }>('/settings', { method: 'PUT', body: body(v) }),
  runCleanup: () => request<{ ok: boolean; cleaner: CleanerStatus }>('/settings/cleanup', { method: 'POST' }),

  users: () => request<User[]>('/users'),
  updateUser: (id: number, v: { role?: string; group_id?: number; enabled?: boolean; password?: string }) =>
    request<User>(`/users/${id}`, { method: 'PUT', body: body(v) }),
  deleteUser: (id: number) => request<void>(`/users/${id}`, { method: 'DELETE' }),
}
