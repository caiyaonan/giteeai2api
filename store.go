package main

import (
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS tokens(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL DEFAULT '',
  token TEXT NOT NULL UNIQUE,
  enabled INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL DEFAULT 'active',
  daily_limit INTEGER NOT NULL DEFAULT 0,
  used_today INTEGER NOT NULL DEFAULT 0,
  used_date TEXT NOT NULL DEFAULT '',
  success_total INTEGER NOT NULL DEFAULT 0,
  fail_total INTEGER NOT NULL DEFAULT 0,
  last_used_at INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS api_keys(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL DEFAULT '',
  key TEXT NOT NULL UNIQUE,
  enabled INTEGER NOT NULL DEFAULT 1,
  used_total INTEGER NOT NULL DEFAULT 0,
  last_used_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS logs(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts INTEGER NOT NULL,
  key_id INTEGER NOT NULL DEFAULT 0,
  token_id INTEGER NOT NULL DEFAULT 0,
  endpoint TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  upstream_model TEXT NOT NULL DEFAULT '',
  size TEXT NOT NULL DEFAULT '',
  status INTEGER NOT NULL DEFAULT 0,
  latency_ms INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_logs_ts ON logs(ts);
`

// Store：读写分离。写入走单协程批量事务，避免高并发下 SQLite 锁竞争。
type Store struct {
	r  *sql.DB
	w  *sql.DB
	ch chan func(*sql.Tx)
}

func openStore(dir string) *Store {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatal(err)
	}
	dsn := "file:" + filepath.Join(dir, "data.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatal(err)
	}
	w.SetMaxOpenConns(1)
	if _, err := w.Exec(schema); err != nil {
		log.Fatal("init schema: ", err)
	}
	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatal(err)
	}
	r.SetMaxOpenConns(8)
	s := &Store{r: r, w: w, ch: make(chan func(*sql.Tx), 8192)}
	go s.writer()
	return s
}

// Async 异步写入（日志、计数等允许毫秒级延迟落盘的数据）
func (s *Store) Async(fn func(*sql.Tx)) {
	select {
	case s.ch <- fn:
	default:
		log.Println("store: write queue full, dropping write")
	}
}

func (s *Store) writer() {
	for fn := range s.ch {
		tx, err := s.w.Begin()
		if err != nil {
			log.Println("store begin:", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		fn(tx)
		// 合并队列中已有的写入到同一事务
	drain:
		for i := 0; i < 500; i++ {
			select {
			case f := <-s.ch:
				f(tx)
			default:
				break drain
			}
		}
		if err := tx.Commit(); err != nil {
			log.Println("store commit:", err)
		}
	}
}

func execTx(tx *sql.Tx, q string, args ...any) {
	if _, err := tx.Exec(q, args...); err != nil {
		log.Println("store exec:", err, q)
	}
}
