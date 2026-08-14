// Package registry 把「路由所需的配置」整体缓存在内存里，让转发热路径不再查库。
//
// 原先每个请求要 7 次 SELECT（网关 key、用户、归属、模型、绑定+渠道、上游 key）。
// 这些表都很小、改动很少，所以做法是：一次性把它们组装成一个只读快照，
// 用 atomic.Pointer 发布；热路径只做 map 查找，零锁零查询。
//
// 失效方式：任何管理 API 的写操作成功后调用 Invalidate()（同步重建，保证读到自己的写），
// 另外有 30s 的兜底刷新，防止有人绕过 API 直接改库。
package registry

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	"gorm.io/gorm"
)

// Caller 网关 key 对应的调用方身份（key + 用户 + 归属，一次查找拿全）。
type Caller struct {
	APIKeyID      uint
	APIKeyName    string
	APIKeyEnabled bool
	UserID        uint
	Username      string
	UserEnabled   bool
	GroupID       uint
	GroupName     string
	GroupEnabled  bool
}

// ModelRoute 一个网关模型名的可路由信息。
type ModelRoute struct {
	ID       uint
	Name     string
	Enabled  bool
	Bindings []store.Binding // Channel 已填充
}

// Snapshot 只读快照。
type Snapshot struct {
	BuiltAt time.Time

	callers map[string]*Caller                  // 网关 key -> 调用方
	models  map[string]*ModelRoute              // 网关模型名 -> 路由
	keys    map[channelGroup][]store.ChannelKey // (渠道, 归属) -> 可用上游 key
}

type channelGroup struct {
	channelID uint
	groupID   uint
}

func (s *Snapshot) Caller(gatewayKey string) (*Caller, bool) {
	c, ok := s.callers[gatewayKey]
	return c, ok
}

func (s *Snapshot) Model(name string) (*ModelRoute, bool) {
	m, ok := s.models[name]
	return m, ok
}

// ChannelKeys 返回某渠道在某归属下**已启用**的 key。
func (s *Snapshot) ChannelKeys(channelID, groupID uint) []store.ChannelKey {
	return s.keys[channelGroup{channelID, groupID}]
}

// HasKeyFor 该渠道在该归属下是否有可用 key（决定这条绑定能不能参与路由）。
func (s *Snapshot) HasKeyFor(channelID, groupID uint) bool {
	return len(s.keys[channelGroup{channelID, groupID}]) > 0
}

// Registry 快照持有者。
type Registry struct {
	db   *gorm.DB
	snap atomic.Pointer[Snapshot]

	mu      sync.Mutex // 串行化重建
	reloads atomic.Uint64
}

func New(db *gorm.DB) (*Registry, error) {
	r := &Registry{db: db}
	if err := r.Invalidate(); err != nil {
		return nil, err
	}
	return r, nil
}

// Get 热路径调用：一次原子读，无锁。
func (r *Registry) Get() *Snapshot { return r.snap.Load() }

// Invalidate 同步重建快照。由管理 API 的写操作触发。
func (r *Registry) Invalidate() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	snap, err := r.build()
	if err != nil {
		return err
	}
	r.snap.Store(snap)
	r.reloads.Add(1)
	return nil
}

// StartRefresher 兜底定时刷新。
func (r *Registry) StartRefresher(ctx context.Context, every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := r.Invalidate(); err != nil {
					log.Printf("[registry] 定时刷新失败: %v", err)
				}
			}
		}
	}()
}

// Stats 给 WebUI 看的缓存状态。
func (r *Registry) Stats() map[string]any {
	s := r.Get()
	return map[string]any{
		"built_at": s.BuiltAt.Format(time.RFC3339),
		"reloads":  r.reloads.Load(),
		"callers":  len(s.callers),
		"models":   len(s.models),
		"key_sets": len(s.keys),
	}
}

func (r *Registry) build() (*Snapshot, error) {
	var (
		groups   []store.Group
		users    []store.User
		apiKeys  []store.APIKey
		channels []store.Channel
		chKeys   []store.ChannelKey
		models   []store.Model
		bindings []store.Binding
	)
	if err := r.db.Find(&groups).Error; err != nil {
		return nil, err
	}
	if err := r.db.Find(&users).Error; err != nil {
		return nil, err
	}
	if err := r.db.Find(&apiKeys).Error; err != nil {
		return nil, err
	}
	if err := r.db.Find(&channels).Error; err != nil {
		return nil, err
	}
	if err := r.db.Where("enabled").Find(&chKeys).Error; err != nil {
		return nil, err
	}
	if err := r.db.Find(&models).Error; err != nil {
		return nil, err
	}
	if err := r.db.Where("enabled").Find(&bindings).Error; err != nil {
		return nil, err
	}

	groupByID := make(map[uint]*store.Group, len(groups))
	for i := range groups {
		groupByID[groups[i].ID] = &groups[i]
	}
	userByID := make(map[uint]*store.User, len(users))
	for i := range users {
		userByID[users[i].ID] = &users[i]
	}
	channelByID := make(map[uint]*store.Channel, len(channels))
	for i := range channels {
		ch := channels[i]
		ch.ProtocolList = store.SplitProtocols(ch.Protocols)
		channelByID[ch.ID] = &ch
	}

	callers := make(map[string]*Caller, len(apiKeys))
	for i := range apiKeys {
		k := apiKeys[i]
		c := &Caller{
			APIKeyID: k.ID, APIKeyName: k.Name, APIKeyEnabled: k.Enabled,
			UserID: k.UserID,
		}
		if u := userByID[k.UserID]; u != nil {
			c.Username, c.UserEnabled, c.GroupID = u.Username, u.Enabled, u.GroupID
			if g := groupByID[u.GroupID]; g != nil {
				c.GroupName, c.GroupEnabled = g.Name, g.Enabled
			}
		}
		callers[k.Key] = c
	}

	keys := make(map[channelGroup][]store.ChannelKey, len(chKeys))
	for i := range chKeys {
		k := chKeys[i]
		if ch := channelByID[k.ChannelID]; ch == nil || !ch.Enabled {
			continue // 渠道禁用 -> 这些 key 不参与路由
		}
		if g := groupByID[k.GroupID]; g == nil || !g.Enabled {
			continue // 归属禁用 -> 同理
		}
		cg := channelGroup{k.ChannelID, k.GroupID}
		keys[cg] = append(keys[cg], k)
	}

	bindingsByModel := make(map[uint][]store.Binding, len(models))
	for i := range bindings {
		b := bindings[i]
		ch := channelByID[b.ChannelID]
		if ch == nil || !ch.Enabled {
			continue
		}
		b.Channel = ch
		bindingsByModel[b.ModelID] = append(bindingsByModel[b.ModelID], b)
	}

	routes := make(map[string]*ModelRoute, len(models))
	for i := range models {
		m := models[i]
		routes[m.Name] = &ModelRoute{ID: m.ID, Name: m.Name, Enabled: m.Enabled, Bindings: bindingsByModel[m.ID]}
	}

	return &Snapshot{BuiltAt: time.Now(), callers: callers, models: routes, keys: keys}, nil
}
