package httpapi

import (
	"net/http"

	"github.com/RailyW/go-llm-gateway/backend/internal/archive"
	"github.com/RailyW/go-llm-gateway/backend/internal/cleaner"
	"github.com/RailyW/go-llm-gateway/backend/internal/config"
	"github.com/RailyW/go-llm-gateway/backend/internal/coord"
	"github.com/RailyW/go-llm-gateway/backend/internal/rds"
	"github.com/RailyW/go-llm-gateway/backend/internal/registry"
	"github.com/RailyW/go-llm-gateway/backend/internal/relay"
	"github.com/RailyW/go-llm-gateway/backend/internal/sink"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type Server struct {
	cfg      *config.Config
	db       *gorm.DB
	relay    *relay.Service
	archiver *archive.Archiver
	cleaner  *cleaner.Cleaner
	reg      *registry.Registry
	sink     sink.Sink
	rc       *rds.Client
	inval    *coord.Invalidator
	hb       *coord.Heartbeat
	consumer *sink.Consumer
}

// Deps 构造 Server 需要的依赖。用结构体而不是长参数列表：
// 参数已经到 9 个了，而且按角色有些是 nil（比如 console 不持有 relay）。
type Deps struct {
	Cfg       *config.Config
	DB        *gorm.DB
	Relay     *relay.Service
	Archiver  *archive.Archiver
	Cleaner   *cleaner.Cleaner
	Registry  *registry.Registry
	Sink      sink.Sink
	Redis     *rds.Client
	Inval     *coord.Invalidator
	Heartbeat *coord.Heartbeat
	// Consumer worker/all 角色的 Stream 消费者（其他角色为 nil）
	Consumer *sink.Consumer
}

func NewServer(d Deps) *Server {
	return &Server{
		cfg: d.Cfg, db: d.DB, relay: d.Relay, archiver: d.Archiver,
		cleaner: d.Cleaner, reg: d.Registry, sink: d.Sink, rc: d.Redis, inval: d.Inval, hb: d.Heartbeat,
		consumer: d.Consumer,
	}
}

// Router 装配路由。**按角色装配**：gateway 只挂 /v1/*，console 只挂 /api/* + WebUI。
// 这样转发实例不会意外暂露管理接口，也不需要带 WebUI。
func (s *Server) Router() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	role := s.cfg.Role
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"ok":       true,
			"role":     string(role),
			"instance": s.cfg.InstanceID,
		})
	})

	if role.ServesRelay() {
		// ---------- 协议网关：每个协议一个端点，同协议直转 ----------
		// /v1/chat/completions、/v1/responses、/v1/messages ... 由协议注册表自动生成
		v1 := r.Group("", s.GatewayAuth())
		for _, p := range relay.Protocols() {
			proto := p
			v1.POST(proto.InboundPath(), func(c *gin.Context) {
				s.relay.Handle(proto, c.Writer, c.Request, currentActor(c))
			})
		}
		v1.GET("/v1/models", s.listGatewayModels)
	}

	if !role.ServesConsole() {
		return r
	}

	// ---------- 管理 API（JWT）----------
	// 任何写操作成功后同步重建本地配置快照 + 广播给其他实例
	api := r.Group("/api", s.invalidateRegistryOnWrite())
	{
		api.POST("/auth/register", s.register)
		api.POST("/auth/login", s.login)
		api.GET("/meta", s.meta)

		authed := api.Group("", s.JWTAuth())
		authed.GET("/auth/me", s.me)
		authed.POST("/auth/password", s.changePassword)

		// 自己的 API key
		authed.GET("/keys", s.listKeys)
		authed.POST("/keys", s.createKey)
		authed.PUT("/keys/:id", s.updateKey)
		authed.DELETE("/keys/:id", s.deleteKey)

		// 日志：普通用户只看自己的，admin 看全部
		authed.GET("/logs", s.listLogs)
		authed.GET("/logs/:id", s.getLog)
		authed.GET("/logs/:id/archive", s.getLogArchive)
		authed.GET("/stats", s.stats)

		admin := authed.Group("", s.AdminOnly())
		admin.GET("/channels", s.listChannels)
		admin.POST("/channels", s.createChannel)
		admin.PUT("/channels/:id", s.updateChannel)
		admin.DELETE("/channels/:id", s.deleteChannel)

		admin.GET("/channels/:id/keys", s.listChannelKeys)
		admin.POST("/channels/:id/keys", s.createChannelKey)
		admin.PUT("/channel-keys/:id", s.updateChannelKey)
		admin.DELETE("/channel-keys/:id", s.deleteChannelKey)

		admin.GET("/groups", s.listGroups)
		admin.POST("/groups", s.createGroup)
		admin.PUT("/groups/:id", s.updateGroup)
		admin.DELETE("/groups/:id", s.deleteGroup)

		admin.GET("/models", s.listModels)
		admin.POST("/models", s.createModel)
		admin.PUT("/models/:id", s.updateModel)
		admin.DELETE("/models/:id", s.deleteModel)

		admin.POST("/models/:id/bindings", s.createBinding)
		admin.PUT("/bindings/:id", s.updateBinding)
		admin.DELETE("/bindings/:id", s.deleteBinding)

		admin.GET("/settings", s.getSettings)
		admin.PUT("/settings", s.updateSettings)
		admin.POST("/settings/cleanup", s.triggerCleanup)

		admin.GET("/users", s.listUsers)
		admin.PUT("/users/:id", s.updateUser)
		admin.DELETE("/users/:id", s.deleteUser)
	}

	s.mountWeb(r)
	return r
}

func fail(c *gin.Context, status int, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"error": msg})
}
