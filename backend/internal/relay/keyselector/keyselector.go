// Package keyselector 决定「某个上游 + 某个归属」下的多把 key 用哪一把。
//
// 当前只实现 random。之后要做**亲和性**（比如同一用户/同一会话固定打到同一把 key，
// 以命中上游的 prompt cache）时，新增一个实现 Selector 的类型并 Register 即可，
// 需要的上下文（用户、网关 key、模型、AffinityKey 等）都已经在 Context 里预留好了。
package keyselector

import (
	"errors"
	"hash/fnv"
	"math/rand"
	"sort"
	"sync"

	"github.com/rin/go-llm-gateway/backend/internal/store"
)

var ErrNoKey = errors.New("no available upstream key")

// Context 选 key 时可用的上下文（预留给亲和性策略）。
type Context struct {
	ChannelID     uint
	GroupID       uint
	UserID        uint
	GatewayKeyID  uint   // 调用方使用的网关 key
	Model         string // 网关模型名
	UpstreamModel string
	Protocol      string
	// AffinityKey 亲和性维度：默认取网关 key/用户，将来可换成会话 id、prompt 前缀 hash 等
	AffinityKey string
}

// Selector 上游 key 选择策略。keys 保证非空且均为 enabled。
type Selector interface {
	Name() string
	Select(ctx Context, keys []store.ChannelKey) (*store.ChannelKey, error)
}

var (
	mu         sync.RWMutex
	strategies = map[string]Selector{}
)

func Register(s Selector) {
	mu.Lock()
	defer mu.Unlock()
	strategies[s.Name()] = s
}

// Get 取策略，未知策略回退 random。
func Get(name string) Selector {
	mu.RLock()
	defer mu.RUnlock()
	if s, ok := strategies[name]; ok {
		return s
	}
	return strategies["random"]
}

func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(strategies))
	for k := range strategies {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func init() {
	Register(&Random{})
	Register(&Weighted{})
	Register(&AffinityHash{})
}

// Random 等概率随机。
type Random struct{}

func (r *Random) Name() string { return "random" }

func (r *Random) Select(_ Context, keys []store.ChannelKey) (*store.ChannelKey, error) {
	if len(keys) == 0 {
		return nil, ErrNoKey
	}
	return &keys[rand.Intn(len(keys))], nil
}

// Weighted 按 ChannelKey.Weight 加权随机。
type Weighted struct{}

func (w *Weighted) Name() string { return "weighted" }

func (w *Weighted) Select(_ Context, keys []store.ChannelKey) (*store.ChannelKey, error) {
	if len(keys) == 0 {
		return nil, ErrNoKey
	}
	total := 0
	for i := range keys {
		total += weightOf(keys[i])
	}
	n := rand.Intn(total)
	for i := range keys {
		if n < weightOf(keys[i]) {
			return &keys[i], nil
		}
		n -= weightOf(keys[i])
	}
	return &keys[len(keys)-1], nil
}

// AffinityHash 按 AffinityKey 做一致的取模，同一个调用方总是打到同一把 key
// （上游 prompt cache 友好）。AffinityKey 为空时退化成随机。
type AffinityHash struct{}

func (a *AffinityHash) Name() string { return "affinity-hash" }

func (a *AffinityHash) Select(ctx Context, keys []store.ChannelKey) (*store.ChannelKey, error) {
	if len(keys) == 0 {
		return nil, ErrNoKey
	}
	if ctx.AffinityKey == "" {
		return (&Random{}).Select(ctx, keys)
	}
	// 按 id 排序保证同一集合下取模结果稳定
	sorted := make([]store.ChannelKey, len(keys))
	copy(sorted, keys)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	h := fnv.New32a()
	_, _ = h.Write([]byte(ctx.AffinityKey))
	return &sorted[int(h.Sum32())%len(sorted)], nil
}

func weightOf(k store.ChannelKey) int {
	if k.Weight > 0 {
		return k.Weight
	}
	return 1
}
