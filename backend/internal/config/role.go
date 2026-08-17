package config

import "strings"

// Role 进程角色。同一个二进制通过 GATEWAY_ROLE 切换职责，
// 这样横向扩展只是「多起几个进程、改个环境变量」，不需要维护多个二进制。
//
//	all      单进程全功能（默认）。零依赖体验不丢，本地开发与小规模部署用这个
//	gateway  只转发 /v1/*：无 WebUI、无管理 API、不跑清理任务、PG 连接池很小
//	         （热路径靠内存快照，PG 只在启动和配置失效时读）。**这个角色可以随意扩**
//	console  只提供管理 API + WebUI，不接转发流量
//	worker   不监听业务端口，只消费日志队列 + 跑清理任务（需要选主）
//
// 拆角色的目的：让转发实例变成**无状态、可随时杀、不持有写职责**的东西。
type Role string

const (
	RoleAll     Role = "all"
	RoleGateway Role = "gateway"
	RoleConsole Role = "console"
	RoleWorker  Role = "worker"
)

func parseRole(s string) Role {
	switch Role(strings.ToLower(strings.TrimSpace(s))) {
	case RoleGateway:
		return RoleGateway
	case RoleConsole:
		return RoleConsole
	case RoleWorker:
		return RoleWorker
	default:
		return RoleAll
	}
}

// ServesRelay 是否对外提供 /v1/* 转发端点。
func (r Role) ServesRelay() bool { return r == RoleAll || r == RoleGateway }

// ServesConsole 是否提供管理 API 与 WebUI。
func (r Role) ServesConsole() bool { return r == RoleAll || r == RoleConsole }

// ServesHTTP 是否需要监听 HTTP 端口。worker 只保留健康检查端口。
func (r Role) ServesHTTP() bool { return r != RoleWorker }

// RunsCleaner 是否参与后台清理。
//
// 多实例下清理任务只能有一个执行者，否则会重复删同一批数据。
// gateway 角色**永不**参与（它要保持无状态、不持有写职责）；
// all/worker 会参与，但需要抢到分布式锁才真正执行（见 cleaner 选主）。
func (r Role) RunsCleaner() bool { return r == RoleAll || r == RoleWorker }

// ConsumesLogs 是否消费日志队列并写库。
func (r Role) ConsumesLogs() bool { return r == RoleAll || r == RoleWorker }

// NeedsRegistry 是否需要配置快照（转发要用；console 自己查库不用快照，
// 但它要负责在写操作后广播失效，所以也持有一份）。
func (r Role) NeedsRegistry() bool { return true }

// Label 中文说明，用于启动日志。
func (r Role) Label() string {
	switch r {
	case RoleGateway:
		return "仅转发（无管理台/不清理，可横向扩展）"
	case RoleConsole:
		return "仅管理台（不接转发流量）"
	case RoleWorker:
		return "仅后台（消费日志队列 + 清理，需选主）"
	default:
		return "单进程全功能"
	}
}
