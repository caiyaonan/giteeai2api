package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const sessionCookie = "g2a_session"

// ---------- 会话（无状态 HMAC 签名 Cookie） ----------

func (a *App) sign(exp string) string {
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte(exp))
	return hex.EncodeToString(m.Sum(nil))
}

func (a *App) adminOK(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	exp, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(a.sign(exp))) {
		return false
	}
	ts, _ := strconv.ParseInt(exp, 10, 64)
	return time.Now().Unix() < ts
}

func (a *App) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.adminOK(r) {
			writeErr(w, 401, "authentication_error", "unauthorized", "请先登录")
			return
		}
		h(w, r)
	}
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	if subtle.ConstantTimeCompare([]byte(in.Password), []byte(a.cfg.AdminPassword)) != 1 {
		time.Sleep(500 * time.Millisecond)
		writeErr(w, 401, "authentication_error", "wrong_password", "密码错误")
		return
	}
	exp := strconv.FormatInt(time.Now().Add(7*24*time.Hour).Unix(), 10)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: exp + "." + a.sign(exp), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 7 * 24 * 3600})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

// ---------- 令牌池 ----------

func (a *App) handleTokenList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"data": a.pool.List(), "summary": a.pool.Summary()})
}

func (a *App) handleTokenAdd(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Tokens     string `json:"tokens"` // 多个令牌，换行/逗号分隔
		Name       string `json:"name"`
		DailyLimit int    `json:"daily_limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "invalid_request_error", "", err.Error())
		return
	}
	n, err := a.pool.Add(strings.Fields(strings.NewReplacer(",", " ", ";", " ").Replace(in.Tokens)), in.Name, in.DailyLimit)
	if err != nil {
		writeErr(w, 500, "server_error", "", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"added": n})
}

func (a *App) handleTokenPatch(w http.ResponseWriter, r *http.Request) {
	var p TokenPatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, 400, "invalid_request_error", "", err.Error())
		return
	}
	if !a.pool.Update(pathID(r), p) {
		writeErr(w, 404, "invalid_request_error", "", "token not found")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) handleTokenBatch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs    []int64 `json:"ids"`
		Action string  `json:"action"` // enable | disable | delete | reset
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "invalid_request_error", "", err.Error())
		return
	}
	if in.Action == "delete" {
		writeJSON(w, 200, map[string]any{"affected": a.pool.Delete(in.IDs)})
		return
	}
	t, f, active := true, false, StatusActive
	n := 0
	for _, id := range in.IDs {
		var p TokenPatch
		switch in.Action {
		case "enable":
			p.Enabled = &t
		case "disable":
			p.Enabled = &f
		case "reset": // 重置为可用并清零今日用量
			p.Status, p.ResetUsage = &active, true
		default:
			writeErr(w, 400, "invalid_request_error", "", "unknown action")
			return
		}
		if a.pool.Update(id, p) {
			n++
		}
	}
	writeJSON(w, 200, map[string]any{"affected": n})
}

// checkToken 校验令牌有效性：有效则恢复 invalid 状态，401 则标记 invalid
func (a *App) checkToken(ctx context.Context, t *Token) (bool, string) {
	err := a.up.Check(ctx, t.Value)
	active, invalid := StatusActive, StatusInvalid
	if err == nil {
		if t.Status == StatusInvalid {
			a.pool.Update(t.ID, TokenPatch{Status: &active})
		}
		return true, "令牌有效"
	}
	var ue *UpErr
	if errors.As(err, &ue) && ue.Out == OutInvalid {
		a.pool.Update(t.ID, TokenPatch{Status: &invalid})
	}
	return false, err.Error()
}

func (a *App) handleTokenCheck(w http.ResponseWriter, r *http.Request) {
	t := a.pool.Get(pathID(r))
	if t == nil {
		writeErr(w, 404, "invalid_request_error", "", "token not found")
		return
	}
	ok, msg := a.checkToken(r.Context(), t)
	writeJSON(w, 200, map[string]any{"ok": ok, "message": msg})
}

func (a *App) handleTokenCheckAll(w http.ResponseWriter, r *http.Request) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	okN, badN := 0, 0
	for _, v := range a.pool.List() {
		t := a.pool.Get(v.ID)
		if t == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			ok, _ := a.checkToken(r.Context(), t)
			mu.Lock()
			if ok {
				okN++
			} else {
				badN++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	writeJSON(w, 200, map[string]any{"ok": okN, "failed": badN})
}

// ---------- 客户端密钥 ----------

func (a *App) handleKeyList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"data": a.keys.List(), "static_count": len(a.cfg.APIKeys)})
}

func (a *App) handleKeyCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	k, err := a.keys.Create(in.Name)
	if err != nil {
		writeErr(w, 500, "server_error", "", err.Error())
		return
	}
	writeJSON(w, 200, k)
}

func (a *App) handleKeyPatch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    *string `json:"name"`
		Enabled *bool   `json:"enabled"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	writeJSON(w, 200, map[string]any{"ok": a.keys.Update(pathID(r), in.Name, in.Enabled)})
}

func (a *App) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": a.keys.Delete(pathID(r))})
}

// ---------- 统计 ----------

func startOfToday() int64 {
	n := time.Now().In(cst)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, cst).Unix()
}

type bucket struct {
	T       int64 `json:"t"`
	Total   int   `json:"total"`
	Success int   `json:"success"`
	Fail    int   `json:"fail"`
}

func (a *App) series(since, step int64) []bucket {
	n := int((time.Now().Unix()-since)/step) + 1
	out := make([]bucket, n)
	for i := range out {
		out[i].T = since + int64(i)*step
	}
	rows, err := a.store.r.Query(`SELECT (ts-?)/? AS b, COUNT(*), SUM(status=200) FROM logs WHERE ts>=? GROUP BY b`, since, step, since)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var b, total, succ int
		rows.Scan(&b, &total, &succ)
		if b >= 0 && b < n {
			out[b].Total, out[b].Success, out[b].Fail = total, succ, total-succ
		}
	}
	return out
}

func (a *App) handleOverview(w http.ResponseWriter, r *http.Request) {
	today := startOfToday()
	var total, succ, allTotal, allSucc int
	var avgLat float64
	a.store.r.QueryRow(`SELECT COUNT(*), COALESCE(SUM(status=200),0), COALESCE(AVG(CASE WHEN status=200 THEN latency_ms END),0)
		FROM logs WHERE ts>=?`, today).Scan(&total, &succ, &avgLat)
	a.store.r.QueryRow(`SELECT COUNT(*), COALESCE(SUM(status=200),0) FROM logs`).Scan(&allTotal, &allSucc)

	type kv struct {
		Name    string `json:"name"`
		Total   int    `json:"total"`
		Success int    `json:"success"`
	}
	group := func(q string) []kv {
		var out []kv
		rows, err := a.store.r.Query(q, today)
		if err != nil {
			return out
		}
		defer rows.Close()
		for rows.Next() {
			var x kv
			rows.Scan(&x.Name, &x.Total, &x.Success)
			out = append(out, x)
		}
		return out
	}
	models := group(`SELECT model, COUNT(*), SUM(status=200) FROM logs WHERE ts>=? GROUP BY model ORDER BY 2 DESC LIMIT 10`)
	keyRows := group(`SELECT CAST(key_id AS TEXT), COUNT(*), SUM(status=200) FROM logs WHERE ts>=? GROUP BY key_id ORDER BY 2 DESC LIMIT 10`)
	names := a.keys.Names()
	for i := range keyRows {
		id, _ := strconv.ParseInt(keyRows[i].Name, 10, 64)
		if n, ok := names[id]; ok && n != "" {
			keyRows[i].Name = n
		} else {
			keyRows[i].Name = "#" + keyRows[i].Name
		}
	}
	errs := group(`SELECT error, COUNT(*), 0 FROM logs WHERE ts>=? AND status<>200 GROUP BY error ORDER BY 2 DESC LIMIT 8`)

	now := time.Now().Unix()
	writeJSON(w, 200, map[string]any{
		"pool": a.pool.Summary(),
		"today": map[string]any{
			"total": total, "success": succ, "fail": total - succ, "avg_latency_ms": int(avgLat),
		},
		"all":    map[string]any{"total": allTotal, "success": allSucc},
		"hourly": a.series(now-now%3600-23*3600, 3600),
		"daily":  a.series(today-13*86400, 86400),
		"models": models,
		"keys":   keyRows,
		"errors": errs,
	})
}

func (a *App) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	where, args := "1=1", []any{}
	switch q.Get("status") {
	case "ok":
		where += " AND status=200"
	case "fail":
		where += " AND status<>200"
	}
	if v := q.Get("token_id"); v != "" {
		where += " AND token_id=?"
		args = append(args, v)
	}
	if v := q.Get("key_id"); v != "" {
		where += " AND key_id=?"
		args = append(args, v)
	}
	var total int
	a.store.r.QueryRow(`SELECT COUNT(*) FROM logs WHERE `+where, args...).Scan(&total)
	rows, err := a.store.r.Query(`SELECT id,ts,key_id,token_id,endpoint,model,upstream_model,size,status,latency_ms,attempts,error
		FROM logs WHERE `+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		writeErr(w, 500, "server_error", "", err.Error())
		return
	}
	defer rows.Close()
	keyNames := a.keys.Names()
	tokNames := map[int64]string{}
	for _, t := range a.pool.List() {
		tokNames[t.ID] = t.Masked
		if t.Name != "" {
			tokNames[t.ID] = t.Name + " (" + t.Masked + ")"
		}
	}
	var out []map[string]any
	for rows.Next() {
		var id, ts, keyID, tokID, lat int64
		var ep, model, upm, size, errMsg string
		var status, attempts int
		rows.Scan(&id, &ts, &keyID, &tokID, &ep, &model, &upm, &size, &status, &lat, &attempts, &errMsg)
		out = append(out, map[string]any{"id": id, "ts": ts, "key": keyNames[keyID], "token": tokNames[tokID], "token_id": tokID,
			"endpoint": ep, "model": model, "upstream_model": upm, "size": size, "status": status,
			"latency_ms": lat, "attempts": attempts, "error": errMsg})
	}
	writeJSON(w, 200, map[string]any{"data": out, "total": total})
}

func (a *App) handleConfig(w http.ResponseWriter, r *http.Request) {
	c := a.cfg
	writeJSON(w, 200, map[string]any{
		"upstream_base": c.UpstreamBase, "default_model": c.DefaultModel, "edit_model": c.EditModel,
		"model_map": c.ModelMap, "daily_limit": c.DailyLimit, "token_concurrency": c.TokenConcurrency,
		"max_retries": c.MaxRetries, "queue_timeout_sec": int(c.QueueTimeout.Seconds()),
		"upstream_timeout_sec": int(c.UpstreamTimeout.Seconds()), "max_side": c.MaxSide,
		"default_size": c.DefaultSize, "quality_steps": c.QualitySteps,
	})
}
