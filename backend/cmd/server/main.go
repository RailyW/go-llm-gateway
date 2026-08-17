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

	// ---------- 日志管道 ----------
	//
	// 三种形态，取决于角色和有没有 Redis：
	//
	//   all/worker          Batch      本地攒批直接落 PG（单进程没必要绕 Redis 一圈）
	//   gateway + Redis     Stream     攒批 XADD 进 Redis Stream，由 worker 落库
	//   gateway 无 Redis    Discard    配置错误（横向扩展必须有 Redis），转发照常但日志丢弃
	//
	// all/worker 在配了 Redis 时**同时**起 Consumer 消费 Stream，
	// 于是「1 个 all + N 个 gateway」和「1 console + N gateway + 1 worker」都能工作。
	var logSink sink.Sink = &sink.Discard{}
	var closers []func(context.Context) error
	var batch *sink.Batch
	var stream *sink.Stream
	var consumer *sink.Consumer

	if role.ConsumesLogs() {
		batch = sink.NewBatch(db, cfg.LogQueueSize)
		batch.Start()
		logSink = batch
		closers = append(closers, batch.Close)
	} else if role.ServesRelay() && rc.Enabled() {
		stream = sink.NewStream(rc, cfg.InstanceID, sink.StreamOptions{
			QueueSize: cfg.LogQueueSize,
			MaxLen:    cfg.LogStreamMaxLen,
		})
		stream.Start()
		logSink = stream
		closers = append(closers, stream.Close)
	}

	if role.ConsumesLogs() && rc.Enabled() {
		consumer = sink.NewConsumer(rc, db, sink.ConsumerOptions{
			Instance: cfg.InstanceID,
			Count:    int64(cfg.LogStreamBatch),
		})
		consumer.Start(ctx)
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
	// TTL 15s、间隔 5s（ttl/3）。这是运维观察多实例的唯一窗口，
	// 间隔太长会看到过期数字然后误判（实测 10s 间隔下压测完看到的还是压测前的快照）。
	hb := coord.NewHeartbeat(rc, cfg.InstanceID, 15*time.Second, func() map[string]any {
		m := map[string]any{
			"role":     string(role),
			"port":     cfg.Port,
			"persists": role.ConsumesLogs(),
		}
		if role.ServesRelay() {
			m["registry"] = reg.Stats()
		}
		m["sink"] = logSink.Stats()
		if consumer != nil {
			m["consumer"] = consumer.Stats()
		}
		return m
	})
	hb.Start(ctx)

	deps := httpapi.Deps{
		Cfg: cfg, DB: db, Relay: relaySvc, Archiver: archiver,
		Cleaner: cl, Registry: reg, Sink: logSink, Redis: rc, Inval: inval,
		Heartbeat: hb, Consumer: consumer,
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
	// 先停止收新请求，再把本地缓冲里剩下的日志刷出去，尽量不丢
	if srv != nil {
		_ = srv.Shutdown(shutdownCtx)
	}
	for _, closeFn := range closers {
		if err := closeFn(shutdownCtx); err != nil {
			log.Printf("[shutdown] 日志缓冲未完全刷完: %v", err)
		}
	}
	if s := logSink.Stats(); s.Active {
		log.Printf("[shutdown] 日志缓冲已刷完（去处 %s：累计入队 %d / 已处理 %d / 丢弃 %d）",
			s.Via, s.Enqueued, s.Persisted, s.Dropped)
	}
	// worker 侧：等消费循环把手上那批落完再退（未 ACK 的会由下一个 worker 接管）
	if consumer != nil {
		if err := consumer.Wait(shutdownCtx); err != nil {
			log.Printf("[shutdown] 日志消费者未干净退出: %v", err)
		}
		s := consumer.Stats()
		log.Printf("[shutdown] 日志消费者已停止（累计落库 %d，剩余积压 %d）", s.Persisted, s.Backlog)
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
	hasRedis := cfg.RedisAddr != ""
	switch {
	case role.ConsumesLogs():
		log.Printf("[boot] 日志直接落库，本地队列 %d 条（只放日志行，~350 字节/条）", cfg.LogQueueSize)
		if hasRedis {
			log.Printf("[boot] 同时消费 Redis Stream 日志（每批最多 %d 条，供 gateway 实例投递）", cfg.LogStreamBatch)
		}
	case role.ServesRelay() && hasRedis:
		log.Printf("[boot] 日志投递到 Redis Stream（本地队列 %d 条，流上限 %d 条），由 worker/all 实例落库",
			cfg.LogQueueSize, cfg.LogStreamMaxLen)
	case role.ServesRelay():
		// gateway 角色没配 Redis 是配置错误：它不直连 PG 写日志（那是解耦的目的），
		// 而没有 Redis 就没有别的去处。不拒绝启动，但必须喊出来。
		log.Printf("[boot] ⚠ 本角色为 gateway 但未配置 Redis —— 转发正常，" +
			"但请求日志无处可去会被丢弃。请配置 GATEWAY_REDIS_ADDR，或改用 GATEWAY_ROLE=all")
	default:
		log.Printf("[boot] 本角色不处理日志")
	}
	if !store.ArchiveFeatureEnabled {
		log.Printf("[boot] 原文归档功能已停用（写本地磁盘，与多实例转发不兼容，待改共享存储）")
	}
}
