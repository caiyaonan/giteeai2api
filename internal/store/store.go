package store

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// migrations 按顺序执行，索引 i 对应将数据库升级到 user_version = i+1。
var migrations = []string{
	// v1: 初始表结构
	`
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
`,
}

// Store 读写分离的 SQLite 存储。异步写入走单协程批量事务，避免高并发下锁竞争。
type Store struct {
	R         *sql.DB
	W         *sql.DB
	ch        chan func(*sql.Tx)
	done      chan struct{}
	closeOnce sync.Once
	mu        sync.RWMutex
	closed    bool
}

func LatestSchemaVersion() int { return len(migrations) }

func Open(dir string) *Store {
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
	if err := migrate(w); err != nil {
		log.Fatal("migrate db: ", err)
	}
	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatal(err)
	}
	r.SetMaxOpenConns(8)
	s := &Store{R: r, W: w, ch: make(chan func(*sql.Tx), 8192), done: make(chan struct{})}
	go s.writer()
	return s
}

func migrate(db *sql.DB) error {
	var cur int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&cur); err != nil {
		return err
	}
	if cur > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than binary (%d), please upgrade", cur, len(migrations))
	}
	for i := cur; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration v%d failed: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		log.Printf("db migrated to schema v%d", i+1)
	}
	return nil
}

// Async 异步写入（日志、计数等允许毫秒级延迟落盘的数据）
func (s *Store) Async(fn func(*sql.Tx)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- fn:
	default:
		log.Println("store: write queue full, dropping write")
	}
}

// Close 刷干所有待写入事务、执行 WAL checkpoint 并关闭数据库连接
func (s *Store) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.ch)
		s.mu.Unlock()
		<-s.done
		_, _ = s.W.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		_ = s.R.Close()
		_ = s.W.Close()
	})
}

func (s *Store) writer() {
	defer close(s.done)
	for fn := range s.ch {
		tx, err := s.W.Begin()
		if err != nil {
			log.Println("store begin:", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		fn(tx)
	drain:
		for i := 0; i < 500; i++ {
			select {
			case f, ok := <-s.ch:
				if !ok {
					break drain
				}
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

func ExecTx(tx *sql.Tx, q string, args ...any) {
	if _, err := tx.Exec(q, args...); err != nil {
		log.Println("store exec:", err, q)
	}
}
