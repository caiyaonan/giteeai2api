package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"giteeai2api/internal/config"
	"giteeai2api/internal/store"
)

// APIKey 下游客户端调用本服务使用的密钥
type APIKey struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Key        string `json:"key"`
	Enabled    bool   `json:"enabled"`
	UsedTotal  int64  `json:"used_total"`
	LastUsedAt int64  `json:"last_used_at"`
	CreatedAt  int64  `json:"created_at"`
}

type KeyStore struct {
	store  *store.Store
	static map[string]bool
	mu     sync.RWMutex
	keys   map[string]*APIKey
}

func New(cfg *config.Config, s *store.Store) *KeyStore {
	ks := &KeyStore{store: s, static: map[string]bool{}, keys: map[string]*APIKey{}}
	for _, k := range cfg.APIKeys {
		ks.static[k] = true
	}
	rows, err := s.R.Query(`SELECT id,name,key,enabled,used_total,last_used_at,created_at FROM api_keys`)
	if err != nil {
		panic(err)
	}
	defer rows.Close()
	for rows.Next() {
		k := &APIKey{}
		rows.Scan(&k.ID, &k.Name, &k.Key, &k.Enabled, &k.UsedTotal, &k.LastUsedAt, &k.CreatedAt)
		ks.keys[k.Key] = k
	}
	return ks
}

func (ks *KeyStore) Auth(r *http.Request) (int64, bool) {
	key := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if key == "" {
		key = r.Header.Get("x-api-key")
	}
	if key == "" {
		return 0, false
	}
	if ks.static[key] {
		return 0, true
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	k := ks.keys[key]
	if k == nil || !k.Enabled {
		return 0, false
	}
	k.UsedTotal++
	k.LastUsedAt = time.Now().Unix()
	id, used, last := k.ID, k.UsedTotal, k.LastUsedAt
	ks.store.Async(func(tx *sql.Tx) {
		store.ExecTx(tx, `UPDATE api_keys SET used_total=?,last_used_at=? WHERE id=?`, used, last, id)
	})
	return id, true
}

func (ks *KeyStore) List() []APIKey {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	out := make([]APIKey, 0, len(ks.keys))
	for _, k := range ks.keys {
		out = append(out, *k)
	}
	return out
}

func (ks *KeyStore) Create(name string) (*APIKey, error) {
	b := make([]byte, 24)
	rand.Read(b)
	k := &APIKey{Name: name, Key: "sk-" + hex.EncodeToString(b), Enabled: true, CreatedAt: time.Now().Unix()}
	res, err := ks.store.W.Exec(`INSERT INTO api_keys(name,key,created_at) VALUES(?,?,?)`, k.Name, k.Key, k.CreatedAt)
	if err != nil {
		return nil, err
	}
	k.ID, _ = res.LastInsertId()
	ks.mu.Lock()
	ks.keys[k.Key] = k
	ks.mu.Unlock()
	return k, nil
}

func (ks *KeyStore) Update(id int64, name *string, enabled *bool) bool {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	for _, k := range ks.keys {
		if k.ID == id {
			if name != nil {
				k.Name = *name
			}
			if enabled != nil {
				k.Enabled = *enabled
			}
			n, e := k.Name, k.Enabled
			ks.store.Async(func(tx *sql.Tx) {
				store.ExecTx(tx, `UPDATE api_keys SET name=?,enabled=? WHERE id=?`, n, e, id)
			})
			return true
		}
	}
	return false
}

func (ks *KeyStore) Delete(id int64) bool {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	for key, k := range ks.keys {
		if k.ID == id {
			delete(ks.keys, key)
			ks.store.Async(func(tx *sql.Tx) {
				store.ExecTx(tx, `DELETE FROM api_keys WHERE id=?`, id)
			})
			return true
		}
	}
	return false
}

func (ks *KeyStore) Names() map[int64]string {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	m := map[int64]string{0: "env"}
	for _, k := range ks.keys {
		m[k.ID] = k.Name
	}
	return m
}
