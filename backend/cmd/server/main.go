package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/RailyW/go-llm-gateway/backend/internal/archive"
	"github.com/RailyW/go-llm-gateway/backend/internal/auth"
	"github.com/RailyW/go-llm-gateway/backend/internal/cleaner"
	"github.com/RailyW/go-llm-gateway/backend/internal/config"
	"github.com/RailyW/go-llm-gateway/backend/internal/coord"
	"github.com/RailyW/go-llm-gateway/backend/internal/httpapi"
	"github.com/RailyW/go-llm-gateway/backend/internal/rds"
	"github.com/RailyW/go-llm-gateway/backend/internal/registry"
	"github.com/RailyW/go-llm-gateway/backend/internal/relay"
	"github.com/RailyW/go-llm-gateway/backend/internal/sink"
	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	webassets "github.com/RailyW/go-llm-gateway/backend/internal/web"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	cfg := config.Load()
	role := cfg.Role

	db, err := store.Open(cfg.DSN, cfg.AdminUser, cfg.AdminPass, store.Options{
		MaxOpen: cfg.DBMaxOpen, MaxIdle: cfg.DBMaxIdle,
	})
	if err != nil {
		log.Fatalf("初始化数据库失败: %v (DSN %s)", err, config.SafeDSN(cfg.DSN))
	}
	if !cfg.AllowRegister {
		_ = store.SetSettings(map[string]string{store.KeyAllowRegister: "false"})
	}
	auth.Init(cfg.JWTSecret)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ---------- Redis（可选，fail-open）----------
	rc := rds.New(rds.Options{
		Addr:      cfg.RedisAddr,
		Password:  cfg.RedisPassword,
		DB:        cfg.RedisDB,
		KeyPrefix: cfg.RedisPrefix,
		Timeout:   time.Duration(cfg.RedisTimeoutMs) * time.Millisecond,
	})
	defer rc.Close()
	if rc.Enabled() {
		if err := rc.Ping(ctx); err != nil {
			// 不 Fatal：fail-open 的语义下 Redis 起得比网关晚是允许的，之后会自动恢复
			log.Printf("[boot] Redis %s 暂时不可用（将以降级模式运行，恢复后自动接入）: %v", cfg.RedisAddr, err)
		} else {
			log.Printf("[boot] Redis %s 已连接（前缀 %s，超时 %dms）", cfg.RedisAddr, cfg.RedisPrefix, cfg.RedisTimeoutMs)
		}
	} else {
		log.Printf("[boot] 未配置 Redis：单实例模式（跨实例配置广播与选主不可用）")
	}

	archiver := archive.NewArchiver(cfg.ArchiveDir)
	if store.ArchiveFeatureEnabled {
		if err := os.MkdirAll(cfg.ArchiveDir, 0o755); err != nil {
			log.Fatalf("创建归档目录失败: %v", err)
		}
	}

	// 配置内存快照：转发热路径不查库。Redis 只负责广播失效，不存配置。
	reg, err := registry.New(db)
	if err != nil {
		log.Fatalf("初始化配置快照失败: %v", err)
	}
	inval := coord.NewInvalidator(rc, cfg.InstanceID, func() {
		if err := reg.Invalidate(); err != nil {
			log.Printf("[registry] 按广播重建配置快照失败: %v", err)
		}
	})
	inval.Subscribe(ctx)
	reg.StartRefresher(ctx, 30*time.Second) // 兜底：广播丢了也最终一致

	// ---------- 日志落库（只有 all/worker 消费）----------
	var logSink sink.Sink = &sink.Discard{}
	var batch *sink.Batch
	if role.ConsumesLogs() {
		batch = sink.NewBatch(db, cfg.LogQueueSize)
		batch.Start()
		logSink = batch
	}

	relaySvc := relay.NewService(reg, logSink, archiver)

	// ---------- 后台清理（需要选主，fail-closed）----------
	var cl *cleaner.Cleaner
	if role.RunsCleaner() {
		// solo：没配 Redis 就不可能有第二个实例，此时坚持选主只会让清理永久不执行
		solo := !rc.Enabled()
		elector := coord.NewElector(rc, "cleaner", cfg.InstanceID, 30*time.Second, solo)
		elector.Run(ctx)
		cl = cleaner.New(db, archiver).WithLeader(elector)
		cl.Start(ctx)
	}

	// 实例心跳：每个实例把自己的状态写进 Redis，Console 聚合展示。
	// 多实例下 /api/stats 只反映本进程，没有这个运维就看不到 gateway 实例的健康度。
	hb := coord.NewHeartbeat(rc, cfg.InstanceID, 30*time.Second, func() map[string]any {
		m := map[string]any{
			"role":     string(role),
			"port":     cfg.Port,
			"persists": role.ConsumesLogs(),
		}
		if role.ServesRelay() {
			m["registry"] = reg.Stats()
		}
		if role.ConsumesLogs() {
			m["sink"] = logSink.Stats()
		} else {
			// gateway 角色当前不落库：日志确实丢了（Streams 未实现），必须暴露出来
			m["logs_dropped"] = logSink.Stats().Dropped
		}
		return m
	})
	hb.Start(ctx)

	deps := httpapi.Deps{
		Cfg: cfg, DB: db, Relay: relaySvc, Archiver: archiver,
		Cleaner: cl, Registry: reg, Sink: logSink, Redis: rc, Inval: inval, Heartbeat: hb,
	}

	logBoot(cfg, role)

	var srv *http.Server
	if role.ServesHTTP() {
		srv = &http.Server{
			Addr:              ":" + cfg.Port,
			Handler:           httpapi.NewServer(deps).Router(),
			ReadHeaderTimeout: 15 * time.Second,
		}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("服务启动失败: %v", err)
			}
		}()
	} else {
		log.Printf("[boot] worker 角色不监听业务端口")
	}

	<-ctx.Done()
	log.Println("[shutdown] 正在退出...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// 先停止收新请求，再把异步队列里剩下的日志刷完，尽量不丢
	if srv != nil {
		_ = srv.Shutdown(shutdownCtx)
	}
	if batch != nil {
		if err := batch.Close(shutdownCtx); err != nil {
			log.Printf("[shutdown] 落库队列未完全刷完: %v", err)
		} else {
			s := batch.Stats()
			log.Printf("[shutdown] 落库队列已刷完（累计入队 %d / 落库 %d / 丢弃 %d）", s.Enqueued, s.Persisted, s.Dropped)
		}
	}
}

func logBoot(cfg *config.Config, role config.Role) {
	log.Printf("[boot] 角色 %s —— %s（实例 %s）", role, role.Label(), cfg.InstanceID)
	if role.ServesHTTP() {
		log.Printf("[boot] 监听 http://localhost:%s  (前端内嵌=%v)", cfg.Port, webassets.Built())
	}
	log.Printf("[boot] 数据库 %s (连接池 %d/%d)", config.SafeDSN(cfg.DSN), cfg.DBMaxIdle, cfg.DBMaxOpen)
	if role.ServesRelay() {
		log.Printf("[boot] 网关端点 POST http://localhost:%s/v1/chat/completions | /v1/responses | /v1/messages", cfg.Port)
	}
	if role.ConsumesLogs() {
		log.Printf("[boot] 异步落库队列容量 %d 条（只放日志行，~350 字节/条）", cfg.LogQueueSize)
	} else if role.ServesRelay() {
		// 这是当前架构的已知缺口：gateway 不连 PG 写日志，但 Redis Streams 还没接，
		// 所以它转发的请求日志是**丢掉**的。必须显式喊出来，不能让人以为一切正常。
		log.Printf("[boot] ⚠ 本角色不落库，且日志队列尚未接入 Redis Streams —— " +
			"该实例转发的请求日志会被丢弃（计数在管理台「集群」里可见）")
	} else {
		log.Printf("[boot] 本角色不落库：日志交给 worker/all 实例处理")
	}
	if !store.ArchiveFeatureEnabled {
		log.Printf("[boot] 原文归档功能已停用（写本地磁盘，与多实例转发不兼容，待改共享存储）")
	}
}
