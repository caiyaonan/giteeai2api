package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func makePNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 100, G: 150, B: 220, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func newTestServer(t *testing.T, upstreamHandler http.HandlerFunc) (*App, *httptest.Server, func()) {
	t.Helper()
	upSrv := httptest.NewServer(upstreamHandler)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "images"), 0o755)
	cfg := loadConfig()
	cfg.DataDir = dir
	cfg.AdminPassword = "secret"
	cfg.UpstreamBase = upSrv.URL + "/v1"
	cfg.DailyLimit = 10
	cfg.TokenConcurrency = 2
	cfg.MaxRetries = 2
	cfg.QueueTimeout = 3 * time.Second
	cfg.UpstreamTimeout = 5 * time.Second

	store := openStore(dir)
	sum := sha256.Sum256([]byte("giteeai2api:" + cfg.AdminPassword))
	app := &App{cfg: cfg, store: store, up: newUpstream(cfg), secret: sum[:]}
	app.pool = newPool(cfg, store)
	app.keys = newKeyStore(cfg, store)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/generations", app.handleGenerations)
	mux.HandleFunc("POST /v1/images/edits", app.handleEdits)
	mux.HandleFunc("GET /v1/models", app.handleModels)
	mux.Handle("GET /images/", http.StripPrefix("/images/", noDirFS(filepath.Join(dir, "images"))))
	mux.HandleFunc("POST /admin/api/login", app.handleLogin)
	mux.HandleFunc("GET /admin/api/overview", app.admin(app.handleOverview))
	mux.HandleFunc("GET /admin/api/tokens", app.admin(app.handleTokenList))
	mux.HandleFunc("POST /admin/api/tokens", app.admin(app.handleTokenAdd))
	mux.HandleFunc("POST /admin/api/tokens/{id}/check", app.admin(app.handleTokenCheck))
	mux.HandleFunc("POST /admin/api/keys", app.admin(app.handleKeyCreate))
	mux.HandleFunc("GET /admin/api/logs", app.admin(app.handleLogs))

	srv := httptest.NewServer(cors(mux))
	cleanup := func() {
		srv.Close()
		upSrv.Close()
		store.Close()
	}
	return app, srv, cleanup
}

func TestEndToEnd(t *testing.T) {
	samplePNG := makePNG(16, 16)
	sampleB64 := base64.StdEncoding.EncodeToString(samplePNG)

	// 模拟 Gitee AI 上游：bad-tok 返回 401，quota-tok 返回 429 额度耗尽，good-tok 成功
	up := func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch auth {
		case "bad-tok":
			w.WriteHeader(401)
			w.Write([]byte(`{"error":{"message":"Unauthorized"}}`))
			return
		case "quota-tok":
			w.WriteHeader(429)
			w.Write([]byte(`{"error":{"message":"今日免费额度已用尽"}}`))
			return
		case "good-tok":
		default:
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"data":[{"id":"z-image-turbo"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"b64_json": sampleB64}},
		})
	}

	_, srv, cleanup := newTestServer(t, up)
	defer cleanup()
	hc := srv.Client()

	// 1. 管理员登录
	resp, err := hc.Post(srv.URL+"/admin/api/login", "application/json", strings.NewReader(`{"password":"secret"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("login failed: %v status=%v", err, resp)
	}
	cookies := resp.Cookies()
	resp.Body.Close()

	adminReq := func(method, path, body string) *http.Response {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for _, c := range cookies {
			req.AddCookie(c)
		}
		r, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return r
	}

	// 2. 导入 3 个令牌（含失效与耗尽，验证自动故障转移）
	r := adminReq("POST", "/admin/api/tokens", `{"tokens":"bad-tok\nquota-tok\ngood-tok","name":"batch"}`)
	var addRes struct{ Added int }
	json.NewDecoder(r.Body).Decode(&addRes)
	r.Body.Close()
	if addRes.Added != 3 {
		t.Fatalf("expected 3 tokens added, got %d", addRes.Added)
	}

	// 3. 创建客户端 API Key
	r = adminReq("POST", "/admin/api/keys", `{"name":"test-client"}`)
	var keyRes APIKey
	json.NewDecoder(r.Body).Decode(&keyRes)
	r.Body.Close()
	if !strings.HasPrefix(keyRes.Key, "sk-") {
		t.Fatalf("unexpected api key: %+v", keyRes)
	}

	// 4. 调用 /v1/models
	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+keyRes.Key)
	r, err = hc.Do(req)
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("/v1/models failed: %v %v", err, r)
	}
	r.Body.Close()

	// 5. 文生图（gpt-image-2 + jpeg 转码，验证自动跳过 bad-tok 与 quota-tok 后由 good-tok 成功）
	genReq, _ := http.NewRequest("POST", srv.URL+"/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-2","prompt":"a cute cat","size":"1024x1536","quality":"high","output_format":"jpeg","n":2}`))
	genReq.Header.Set("Authorization", "Bearer "+keyRes.Key)
	genReq.Header.Set("Content-Type", "application/json")
	r, err = hc.Do(genReq)
	if err != nil || r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("generations failed: %v status=%d body=%s", err, r.StatusCode, b)
	}
	var genRes struct {
		Data         []struct{ B64 string `json:"b64_json"` } `json:"data"`
		OutputFormat string                                   `json:"output_format"`
		Size         string                                   `json:"size"`
		Quality      string                                   `json:"quality"`
		Usage        map[string]any                           `json:"usage"`
	}
	json.NewDecoder(r.Body).Decode(&genRes)
	r.Body.Close()
	if len(genRes.Data) != 2 || genRes.OutputFormat != "jpeg" || genRes.Size != "1024x1536" || genRes.Quality != "high" {
		t.Fatalf("unexpected genRes: %+v", genRes)
	}
	rawImg, _ := base64.StdEncoding.DecodeString(genRes.Data[0].B64)
	if detectFormat(rawImg) != "jpeg" {
		t.Fatalf("expected jpeg bytes, got %s", detectFormat(rawImg))
	}

	// 6. 文生图 response_format=url
	urlReq, _ := http.NewRequest("POST", srv.URL+"/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-2","prompt":"sunset","response_format":"url"}`))
	urlReq.Header.Set("Authorization", "Bearer "+keyRes.Key)
	r, _ = hc.Do(urlReq)
	var urlRes struct {
		Data []struct{ URL string `json:"url"` } `json:"data"`
	}
	json.NewDecoder(r.Body).Decode(&urlRes)
	r.Body.Close()
	if len(urlRes.Data) != 1 || !strings.Contains(urlRes.Data[0].URL, "/images/") {
		t.Fatalf("unexpected urlRes: %+v", urlRes)
	}
	imgR, err := hc.Get(urlRes.Data[0].URL)
	if err != nil || imgR.StatusCode != 200 {
		t.Fatalf("download generated image url failed: %v %v", err, imgR)
	}
	imgR.Body.Close()

	// 7. 流式 SSE
	sseReq, _ := http.NewRequest("POST", srv.URL+"/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-2","prompt":"stream test","stream":true}`))
	sseReq.Header.Set("Authorization", "Bearer "+keyRes.Key)
	r, _ = hc.Do(sseReq)
	sseBody, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(sseBody), "event: image_generation.completed") {
		t.Fatalf("unexpected sse body: %s", sseBody)
	}

	// 8. 改图 multipart /v1/images/edits
	var mbuf bytes.Buffer
	mw := multipart.NewWriter(&mbuf)
	mw.WriteField("model", "gpt-image-2")
	mw.WriteField("prompt", "add a hat")
	fw, _ := mw.CreateFormFile("image[]", "in.png")
	fw.Write(samplePNG)
	mw.Close()
	editReq, _ := http.NewRequest("POST", srv.URL+"/v1/images/edits", &mbuf)
	editReq.Header.Set("Authorization", "Bearer "+keyRes.Key)
	editReq.Header.Set("Content-Type", mw.FormDataContentType())
	r, err = hc.Do(editReq)
	if err != nil || r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("edits failed: %v status=%d body=%s", err, r.StatusCode, b)
	}
	r.Body.Close()

	// 9. 验证令牌状态已正确更新（1 active, 1 invalid, 1 exhausted）与统计数据
	time.Sleep(150 * time.Millisecond) // 等待异步落盘
	r = adminReq("GET", "/admin/api/overview", "")
	var ov struct {
		Pool  PoolSummary `json:"pool"`
		Today struct {
			Total   int `json:"total"`
			Success int `json:"success"`
		} `json:"today"`
	}
	json.NewDecoder(r.Body).Decode(&ov)
	r.Body.Close()
	if ov.Pool.Active != 1 || ov.Pool.Invalid != 1 || ov.Pool.Exhausted != 1 {
		t.Fatalf("unexpected pool summary: %+v", ov.Pool)
	}
	if ov.Today.Success != 5 {
		t.Fatalf("expected 5 successful images today, got %+v", ov.Today)
	}
}

func TestStoreMigrationAndGracefulClose(t *testing.T) {
	dir := t.TempDir()
	s := openStore(dir)
	var ver int
	if err := s.r.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil || ver != len(migrations) {
		t.Fatalf("expected schema version %d, got %d (err=%v)", len(migrations), ver, err)
	}
	for i := 0; i < 20; i++ {
		s.Async(func(tx *sql.Tx) {
			execTx(tx, `INSERT INTO logs(ts,endpoint,model,status) VALUES(1,'generations','gpt-image-2',200)`)
		})
	}
	s.Close() // 必须等全部异步写入刷盘完成

	// 重新打开确认 20 条记录全部持久化
	s2 := openStore(dir)
	defer s2.Close()
	var cnt int
	if err := s2.r.QueryRow(`SELECT COUNT(*) FROM logs`).Scan(&cnt); err != nil || cnt != 20 {
		t.Fatalf("expected 20 persisted logs after Close, got %d (err=%v)", cnt, err)
	}
}
