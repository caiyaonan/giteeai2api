package server

import (
	"crypto/sha256"
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"giteeai2api/internal/auth"
	"giteeai2api/internal/config"
	"giteeai2api/internal/pool"
	"giteeai2api/internal/store"
	"giteeai2api/internal/upstream"
	"giteeai2api/web"
)

type BuildInfo struct {
	Version   string
	Commit    string
	BuildTime string
}

type Server struct {
	cfg    *config.Config
	store  *store.Store
	pool   *pool.Pool
	up     *upstream.Client
	keys   *auth.KeyStore
	build  BuildInfo
	secret []byte
}

func New(cfg *config.Config, st *store.Store, p *pool.Pool, up *upstream.Client, ks *auth.KeyStore, bi BuildInfo) *Server {
	sum := sha256.Sum256([]byte("giteeai2api:" + cfg.AdminPassword))
	return &Server{cfg: cfg, store: st, pool: p, up: up, keys: ks, build: bi, secret: sum[:]}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// OpenAI 兼容接口
	mux.HandleFunc("POST /v1/images/generations", s.handleGenerations)
	mux.HandleFunc("POST /v1/images/edits", s.handleEdits)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.Handle("GET /images/", http.StripPrefix("/images/", noDirFS(filepath.Join(s.cfg.DataDir, "images"))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"status": "ok", "version": s.build.Version, "commit": s.build.Commit})
	})

	// 管理后台
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/admin", http.StatusFound) })
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(web.AdminHTML)
	})
	mux.HandleFunc("POST /admin/api/login", s.handleLogin)
	mux.HandleFunc("POST /admin/api/logout", s.handleLogout)
	mux.HandleFunc("GET /admin/api/overview", s.admin(s.handleOverview))
	mux.HandleFunc("GET /admin/api/config", s.admin(s.handleConfig))
	mux.HandleFunc("GET /admin/api/tokens", s.admin(s.handleTokenList))
	mux.HandleFunc("POST /admin/api/tokens", s.admin(s.handleTokenAdd))
	mux.HandleFunc("POST /admin/api/tokens/batch", s.admin(s.handleTokenBatch))
	mux.HandleFunc("POST /admin/api/tokens/check-all", s.admin(s.handleTokenCheckAll))
	mux.HandleFunc("PATCH /admin/api/tokens/{id}", s.admin(s.handleTokenPatch))
	mux.HandleFunc("POST /admin/api/tokens/{id}/check", s.admin(s.handleTokenCheck))
	mux.HandleFunc("GET /admin/api/keys", s.admin(s.handleKeyList))
	mux.HandleFunc("POST /admin/api/keys", s.admin(s.handleKeyCreate))
	mux.HandleFunc("PATCH /admin/api/keys/{id}", s.admin(s.handleKeyPatch))
	mux.HandleFunc("DELETE /admin/api/keys/{id}", s.admin(s.handleKeyDelete))
	mux.HandleFunc("GET /admin/api/logs", s.admin(s.handleLogs))
	return cors(mux)
}

// StartJanitor 定期清理过期缓存图片与旧日志
func (s *Server) StartJanitor() {
	go func() {
		for {
			dir := filepath.Join(s.cfg.DataDir, "images")
			if entries, err := os.ReadDir(dir); err == nil {
				for _, e := range entries {
					if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > s.cfg.ImageTTL {
						_ = os.Remove(filepath.Join(dir, e.Name()))
					}
				}
			}
			before := time.Now().AddDate(0, 0, -s.cfg.LogRetentionDays).Unix()
			s.store.Async(func(tx *sql.Tx) { store.ExecTx(tx, `DELETE FROM logs WHERE ts<?`, before) })
			time.Sleep(time.Hour)
		}
	}()
}

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

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
