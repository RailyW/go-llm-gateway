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
	"github.com/RailyW/go-llm-gateway/backend/internal/httpapi"
	"github.com/RailyW/go-llm-gateway/backend/internal/registry"
	"github.com/RailyW/go-llm-gateway/backend/internal/relay"
	"github.com/RailyW/go-llm-gateway/backend/internal/sink"
	"github.com/RailyW/go-llm-gateway/backend/internal/store"
	webassets "github.com/RailyW/go-llm-gateway/backend/internal/web"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	cfg := config.Load()

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

	archiver := archive.NewArchiver(cfg.ArchiveDir)
	if err := os.MkdirAll(cfg.ArchiveDir, 0o755); err != nil {
		log.Fatalf("创建归档目录失败: %v", err)
	}

	// 配置内存快照：转发热路径不查库
	reg, err := registry.New(db)
	if err != nil {
		log.Fatalf("初始化配置快照失败: %v", err)
	}

	// 异步落库管道：日志与归档都不在请求路径上写
	logSink := sink.NewBatch(db, archiver, cfg.LogQueueSize)
	logSink.Start()

	relaySvc := relay.NewService(reg, logSink, archiver)
	cl := cleaner.New(db, archiver)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cl.Start(ctx)
	reg.StartRefresher(ctx, 30*time.Second)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           httpapi.NewServer(cfg, db, relaySvc, archiver, cl, reg, logSink).Router(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		log.Printf("[boot] 监听 http://localhost:%s  (数据目录 %s, 前端内嵌=%v)", cfg.Port, cfg.DataDir, webassets.Built())
		log.Printf("[boot] 数据库 %s (连接池 %d/%d)", config.SafeDSN(cfg.DSN), cfg.DBMaxIdle, cfg.DBMaxOpen)
		log.Printf("[boot] 网关端点 POST http://localhost:%s/v1/chat/completions | /v1/responses | /v1/messages", cfg.Port)
		log.Printf("[boot] 异步落库队列容量 %d", cfg.LogQueueSize)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("服务启动失败: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("[shutdown] 正在退出...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// 先停止收新请求，再把异步队列里剩下的日志/归档刷完，尽量不丢
	_ = srv.Shutdown(shutdownCtx)
	if err := logSink.Close(shutdownCtx); err != nil {
		log.Printf("[shutdown] 落库队列未完全刷完: %v", err)
	} else {
		s := logSink.Stats()
		log.Printf("[shutdown] 落库队列已刷完（累计入队 %d / 落库 %d / 丢弃 %d）", s.Enqueued, s.Persisted, s.Dropped)
	}
}
