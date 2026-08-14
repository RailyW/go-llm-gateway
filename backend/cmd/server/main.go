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

	"github.com/rin/go-llm-gateway/backend/internal/auth"
	"github.com/rin/go-llm-gateway/backend/internal/cleaner"
	"github.com/rin/go-llm-gateway/backend/internal/config"
	"github.com/rin/go-llm-gateway/backend/internal/httpapi"
	"github.com/rin/go-llm-gateway/backend/internal/relay"
	"github.com/rin/go-llm-gateway/backend/internal/store"
	webassets "github.com/rin/go-llm-gateway/backend/internal/web"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	cfg := config.Load()

	db, err := store.Open(cfg.DBPath, cfg.AdminUser, cfg.AdminPass)
	if err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}
	if !cfg.AllowRegister {
		_ = store.SetSettings(map[string]string{store.KeyAllowRegister: "false"})
	}
	auth.Init(cfg.JWTSecret)

	archiver := relay.NewArchiver(cfg.ArchiveDir)
	if err := os.MkdirAll(cfg.ArchiveDir, 0o755); err != nil {
		log.Fatalf("创建归档目录失败: %v", err)
	}

	relaySvc := relay.NewService(db, archiver)
	cl := cleaner.New(db, archiver)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cl.Start(ctx)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           httpapi.NewServer(cfg, db, relaySvc, archiver, cl).Router(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		log.Printf("[boot] 监听 http://localhost:%s  (数据目录 %s, 前端内嵌=%v)", cfg.Port, cfg.DataDir, webassets.Built())
		log.Printf("[boot] 网关端点 POST http://localhost:%s/v1/chat/completions", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("服务启动失败: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("[shutdown] 正在退出...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
