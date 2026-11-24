package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"giteeai2api/internal/auth"
	"giteeai2api/internal/config"
	"giteeai2api/internal/pool"
	"giteeai2api/internal/server"
	"giteeai2api/internal/store"
	"giteeai2api/internal/upstream"
)

// 构建时可通过 -ldflags 注入：
// go build -ldflags "-X main.Version=v0.1.0 -X main.Commit=abc1234 -X main.BuildTime=2026-11-24T00:00:00Z" ./cmd/giteeai2api
var (
	Version   = "dev"
	Commit    = "none"
	BuildTime = ""
)

func main() {
	cfg := config.Load()
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "images"), 0o755); err != nil {
		log.Fatal(err)
	}
	st := store.Open(cfg.DataDir)
	p := pool.New(cfg, st)
	up := upstream.New(cfg)
	ks := auth.New(cfg, st)
	app := server.New(cfg, st, p, up, ks, server.BuildInfo{
		Version:   Version,
		Commit:    Commit,
		BuildTime: BuildTime,
	})
	app.StartJanitor()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Printf("giteeai2api %s (%s) listening on %s, tokens=%d, admin=/admin", Version, Commit, cfg.Addr, len(p.List()))
		if cfg.AdminPassword == "admin123" {
			log.Println("WARNING: 使用默认管理员密码 admin123，请设置 ADMIN_PASSWORD")
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	st.Close()
}
