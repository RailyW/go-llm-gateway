// Package selector 决定一个模型的多个上游绑定里选哪一个。
//
// 当前只实现 random；之后要加轮询 / 加权 / 最少失败等策略时，
// 新增一个实现 Selector 的类型并 Register 即可，配置项 route_strategy 切换。
package selector

import (
	"errors"
	"math/rand"
	"sort"
	"sync"

	"github.com/RailyW/go-llm-gateway/backend/internal/store"
)

var ErrNoCandidate = errors.New("no available upstream binding")

// Selector 路由策略。candidates 保证非空且 Channel 已预加载。
type Selector interface {
	Name() string
	Select(candidates []store.Binding) (*store.Binding, error)
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

// Get 取策略，未知策略回退到 random。
func Get(name string) Selector {
	mu.RLock()
	defer mu.RUnlock()
	if s, ok := strategies[name]; ok {
		return s
	}
	return strategies["random"]
}

// Names 已注册策略列表，给 WebUI 下拉框用。
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
}

// Random 等概率随机。
type Random struct{}

func (r *Random) Name() string { return "random" }

func (r *Random) Select(candidates []store.Binding) (*store.Binding, error) {
	if len(candidates) == 0 {
		return nil, ErrNoCandidate
	}
	return &candidates[rand.Intn(len(candidates))], nil
}

// Weighted 按 Binding.Weight 加权随机（权重 <=0 视为 1）。
type Weighted struct{}

func (w *Weighted) Name() string { return "weighted" }

func (w *Weighted) Select(candidates []store.Binding) (*store.Binding, error) {
	if len(candidates) == 0 {
		return nil, ErrNoCandidate
	}
	total := 0
	for i := range candidates {
		if candidates[i].Weight > 0 {
			total += candidates[i].Weight
		} else {
			total++
		}
	}
	n := rand.Intn(total)
	for i := range candidates {
		wt := candidates[i].Weight
		if wt <= 0 {
			wt = 1
		}
		if n < wt {
			return &candidates[i], nil
		}
		n -= wt
	}
	return &candidates[len(candidates)-1], nil
}
