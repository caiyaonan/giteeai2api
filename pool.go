package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	StatusActive    = "active"
	StatusExhausted = "exhausted" // 今日额度用尽，次日自动恢复
	StatusInvalid   = "invalid"   // 令牌失效（401），需人工处理
)

// Outcome 一次上游调用的结果，决定令牌状态如何变化
type Outcome int

const (
	OutOK        Outcome = iota
	OutQuota             // 额度用尽
	OutInvalid           // 令牌无效
	OutRate              // 被限流，短暂冷却
	OutTransient         // 网络/5xx，短暂冷却
	OutClient            // 请求本身有问题（参数、内容审核），与令牌无关
)

var (
	ErrNoToken = errors.New("no available token in pool")
	ErrBusy    = errors.New("all tokens are busy, please retry later")
)

type Token struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Value        string `json:"-"`
	Masked       string `json:"token"`
	Enabled      bool   `json:"enabled"`
	Status       string `json:"status"`
	DailyLimit   int    `json:"daily_limit"`
	UsedToday    int    `json:"used_today"`
	SuccessTotal int64  `json:"success_total"`
	FailTotal    int64  `json:"fail_total"`
	LastUsedAt   int64  `json:"last_used_at"`
	LastError    string `json:"last_error"`
	CreatedAt    int64  `json:"created_at"`
	Inflight     int    `json:"inflight"`
	Cooldown     int64  `json:"cooldown_until"`
	// 仅用于展示
	Limit     int `json:"limit"`
	Remaining int `json:"remaining"`
}

type Pool struct {
	cfg    *Config
	store  *Store
	mu     sync.Mutex
	tokens []*Token
	rr     int
	day    string
	wake   chan struct{}
}

func newPool(cfg *Config, s *Store) *Pool {
	p := &Pool{cfg: cfg, store: s, wake: make(chan struct{})}
	rows, err := s.r.Query(`SELECT id,name,token,enabled,status,daily_limit,used_today,used_date,
		success_total,fail_total,last_used_at,last_error,created_at FROM tokens ORDER BY id`)
	if err != nil {
		panic(err)
	}
	defer rows.Close()
	today := todayStr()
	for rows.Next() {
		t := &Token{}
		var usedDate string
		rows.Scan(&t.ID, &t.Name, &t.Value, &t.Enabled, &t.Status, &t.DailyLimit, &t.UsedToday, &usedDate,
			&t.SuccessTotal, &t.FailTotal, &t.LastUsedAt, &t.LastError, &t.CreatedAt)
		if usedDate != today { // 跨天加载：重置当日计数
			t.UsedToday = 0
			if t.Status == StatusExhausted {
				t.Status = StatusActive
			}
		}
		p.tokens = append(p.tokens, t)
	}
	p.day = today
	return p
}

func todayStr() string { return time.Now().In(cst).Format("2006-01-02") }

func (p *Pool) limitOf(t *Token) int {
	if t.DailyLimit > 0 {
		return t.DailyLimit
	}
	return p.cfg.DailyLimit
}

// 调用方需持锁
func (p *Pool) resetIfNewDay() {
	today := todayStr()
	if today == p.day {
		return
	}
	p.day = today
	for _, t := range p.tokens {
		t.UsedToday = 0
		if t.Status == StatusExhausted {
			t.Status = StatusActive
		}
	}
	p.store.Async(func(tx *sql.Tx) {
		execTx(tx, `UPDATE tokens SET used_today=0, used_date=?,
			status=CASE WHEN status='exhausted' THEN 'active' ELSE status END`, today)
	})
	p.notify()
}

func (p *Pool) notify() {
	close(p.wake)
	p.wake = make(chan struct{})
}

// pick 选出可用令牌：优先在途最少，同等条件轮询。busy=true 表示有令牌只是暂时占满，值得等待。
func (p *Pool) pick(exclude map[int64]bool) (best *Token, busy bool) {
	n := len(p.tokens)
	now := time.Now().Unix()
	for i := 0; i < n; i++ {
		t := p.tokens[(p.rr+i)%n]
		if !t.Enabled || t.Status != StatusActive || exclude[t.ID] {
			continue
		}
		limit := p.limitOf(t)
		if limit > 0 && t.UsedToday+t.Inflight >= limit {
			if t.Inflight > 0 { // 在途请求可能失败，从而释放额度
				busy = true
			}
			continue
		}
		if t.Inflight >= p.cfg.TokenConcurrency || t.Cooldown > now {
			busy = true
			continue
		}
		if best == nil || t.Inflight < best.Inflight {
			best = t
		}
	}
	p.rr++
	return
}

func (p *Pool) Acquire(ctx context.Context, exclude map[int64]bool) (*Token, error) {
	timer := time.NewTimer(p.cfg.QueueTimeout)
	defer timer.Stop()
	for {
		p.mu.Lock()
		p.resetIfNewDay()
		t, busy := p.pick(exclude)
		if t != nil {
			t.Inflight++
			t.LastUsedAt = time.Now().Unix()
			p.mu.Unlock()
			return t, nil
		}
		wake := p.wake
		p.mu.Unlock()
		if !busy {
			return nil, ErrNoToken
		}
		select {
		case <-wake:
		case <-time.After(time.Second): // 冷却到期无事件通知，定时复查
		case <-timer.C:
			return nil, ErrBusy
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (p *Pool) Release(t *Token, out Outcome, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t.Inflight--
	now := time.Now().Unix()
	switch out {
	case OutOK:
		t.UsedToday++
		t.SuccessTotal++
	case OutQuota:
		t.Status = StatusExhausted
	case OutInvalid:
		t.Status = StatusInvalid
	case OutRate:
		t.Cooldown = now + 15
	case OutTransient:
		t.Cooldown = now + 5
	}
	if out != OutOK && out != OutClient {
		t.FailTotal++
		t.LastError = truncate(msg, 300)
	}
	p.persist(t)
	p.notify()
}

func (p *Pool) persist(t *Token) {
	used, day, status, st, ft, lu, le := t.UsedToday, p.day, t.Status, t.SuccessTotal, t.FailTotal, t.LastUsedAt, t.LastError
	id := t.ID
	p.store.Async(func(tx *sql.Tx) {
		execTx(tx, `UPDATE tokens SET used_today=?,used_date=?,status=?,success_total=?,fail_total=?,
			last_used_at=?,last_error=? WHERE id=?`, used, day, status, st, ft, lu, le, id)
	})
}

// ---------- 管理操作 ----------

func (p *Pool) List() []Token {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resetIfNewDay()
	out := make([]Token, 0, len(p.tokens))
	for _, t := range p.tokens {
		v := *t
		v.Masked = mask(t.Value)
		v.Limit = p.limitOf(t)
		v.Remaining = -1
		if v.Limit > 0 {
			v.Remaining = max(v.Limit-v.UsedToday, 0)
		}
		out = append(out, v)
	}
	return out
}

func (p *Pool) Get(id int64) *Token {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range p.tokens {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// Add 批量导入，自动去重，返回新增数量
func (p *Pool) Add(values []string, name string, limit int) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	exist := map[string]bool{}
	for _, t := range p.tokens {
		exist[t.Value] = true
	}
	added := 0
	now := time.Now().Unix()
	for _, v := range values {
		v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "Bearer "))
		if v == "" || exist[v] {
			continue
		}
		res, err := p.store.w.Exec(`INSERT INTO tokens(name,token,daily_limit,used_date,created_at) VALUES(?,?,?,?,?)`,
			name, v, limit, p.day, now)
		if err != nil {
			return added, err
		}
		id, _ := res.LastInsertId()
		p.tokens = append(p.tokens, &Token{ID: id, Name: name, Value: v, Enabled: true, Status: StatusActive,
			DailyLimit: limit, CreatedAt: now})
		exist[v] = true
		added++
	}
	p.notify()
	return added, nil
}

type TokenPatch struct {
	Name       *string `json:"name"`
	Enabled    *bool   `json:"enabled"`
	DailyLimit *int    `json:"daily_limit"`
	Status     *string `json:"status"`
	ResetUsage bool    `json:"reset_usage"`
}

func (p *Pool) Update(id int64, f TokenPatch) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range p.tokens {
		if t.ID != id {
			continue
		}
		if f.Name != nil {
			t.Name = *f.Name
		}
		if f.Enabled != nil {
			t.Enabled = *f.Enabled
		}
		if f.DailyLimit != nil {
			t.DailyLimit = *f.DailyLimit
		}
		if f.Status != nil {
			t.Status = *f.Status
			t.Cooldown = 0
		}
		if f.ResetUsage {
			t.UsedToday = 0
		}
		name, en, lim := t.Name, t.Enabled, t.DailyLimit
		p.store.Async(func(tx *sql.Tx) {
			execTx(tx, `UPDATE tokens SET name=?,enabled=?,daily_limit=? WHERE id=?`, name, en, lim, id)
		})
		p.persist(t)
		p.notify()
		return true
	}
	return false
}

func (p *Pool) Delete(ids []int64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	del := map[int64]bool{}
	for _, id := range ids {
		del[id] = true
	}
	kept := p.tokens[:0]
	n := 0
	for _, t := range p.tokens {
		if del[t.ID] {
			n++
			continue
		}
		kept = append(kept, t)
	}
	p.tokens = kept
	p.store.Async(func(tx *sql.Tx) {
		for id := range del {
			execTx(tx, `DELETE FROM tokens WHERE id=?`, id)
		}
	})
	return n
}

type PoolSummary struct {
	Total          int `json:"total"`
	Active         int `json:"active"`
	Exhausted      int `json:"exhausted"`
	Invalid        int `json:"invalid"`
	Disabled       int `json:"disabled"`
	Inflight       int `json:"inflight"`
	UsedToday      int `json:"used_today"`
	RemainingToday int `json:"remaining_today"` // -1 = 不限
}

func (p *Pool) Summary() PoolSummary {
	var s PoolSummary
	unlimited := false
	for _, t := range p.List() {
		s.Total++
		s.Inflight += t.Inflight
		s.UsedToday += t.UsedToday
		switch {
		case !t.Enabled:
			s.Disabled++
			continue
		case t.Status == StatusExhausted:
			s.Exhausted++
		case t.Status == StatusInvalid:
			s.Invalid++
		default:
			s.Active++
			if t.Remaining < 0 {
				unlimited = true
			} else {
				s.RemainingToday += t.Remaining
			}
		}
	}
	if unlimited {
		s.RemainingToday = -1
	}
	return s
}

func mask(s string) string {
	if len(s) <= 10 {
		return "****"
	}
	return s[:4] + "****" + s[len(s)-4:]
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
