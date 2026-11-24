package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

//go:embed web/admin.html
var webFS embed.FS

func main() {
	cfg := loadConfig()
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "images"), 0o755); err != nil {
		log.Fatal(err)
	}
	store := openStore(cfg.DataDir)
	sum := sha256.Sum256([]byte("giteeai2api:" + cfg.AdminPassword))
	app := &App{cfg: cfg, store: store, up: newUpstream(cfg), secret: sum[:]}
	app.pool = newPool(cfg, store)
	app.keys = newKeyStore(cfg, store)
	go app.janitor()

	mux := http.NewServeMux()
	// OpenAI 兼容接口
	mux.HandleFunc("POST /v1/images/generations", app.handleGenerations)
	mux.HandleFunc("POST /v1/images/edits", app.handleEdits)
	mux.HandleFunc("GET /v1/models", app.handleModels)
	mux.Handle("GET /images/", http.StripPrefix("/images/", noDirFS(filepath.Join(cfg.DataDir, "images"))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	// 管理后台
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/admin", http.StatusFound) })
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("web/admin.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	mux.HandleFunc("POST /admin/api/login", app.handleLogin)
	mux.HandleFunc("POST /admin/api/logout", app.handleLogout)
	mux.HandleFunc("GET /admin/api/overview", app.admin(app.handleOverview))
	mux.HandleFunc("GET /admin/api/config", app.admin(app.handleConfig))
	mux.HandleFunc("GET /admin/api/tokens", app.admin(app.handleTokenList))
	mux.HandleFunc("POST /admin/api/tokens", app.admin(app.handleTokenAdd))
	mux.HandleFunc("POST /admin/api/tokens/batch", app.admin(app.handleTokenBatch))
	mux.HandleFunc("POST /admin/api/tokens/check-all", app.admin(app.handleTokenCheckAll))
	mux.HandleFunc("PATCH /admin/api/tokens/{id}", app.admin(app.handleTokenPatch))
	mux.HandleFunc("POST /admin/api/tokens/{id}/check", app.admin(app.handleTokenCheck))
	mux.HandleFunc("GET /admin/api/keys", app.admin(app.handleKeyList))
	mux.HandleFunc("POST /admin/api/keys", app.admin(app.handleKeyCreate))
	mux.HandleFunc("PATCH /admin/api/keys/{id}", app.admin(app.handleKeyPatch))
	mux.HandleFunc("DELETE /admin/api/keys/{id}", app.admin(app.handleKeyDelete))
	mux.HandleFunc("GET /admin/api/logs", app.admin(app.handleLogs))

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           cors(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Printf("giteeai2api listening on %s, tokens=%d, admin=/admin", cfg.Addr, len(app.pool.List()))
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
	srv.Shutdown(ctx)
	time.Sleep(300 * time.Millisecond) // 等待异步写入落盘
}

// cors 允许浏览器直接调用 /v1 接口
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-api-key")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func noDirFS(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
		fs.ServeHTTP(w, r)
	})
}

// janitor 定期清理过期图片与旧日志
func (a *App) janitor() {
	for {
		dir := filepath.Join(a.cfg.DataDir, "images")
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > a.cfg.ImageTTL {
					os.Remove(filepath.Join(dir, e.Name()))
				}
			}
		}
		before := time.Now().AddDate(0, 0, -a.cfg.LogRetentionDays).Unix()
		a.store.Async(func(tx *sql.Tx) { execTx(tx, `DELETE FROM logs WHERE ts<?`, before) })
		time.Sleep(time.Hour)
	}
}
