package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

// CST 北京时间，用于每日额度重置与统计口径
var CST = time.FixedZone("CST", 8*3600)

// Config 全部来自环境变量或本地 .env 文件，见 .env.example
type Config struct {
	Addr             string
	DataDir          string
	AdminPassword    string
	APIKeys          []string
	UpstreamBase     string
	DefaultModel     string
	EditModel        string
	ModelMap         map[string]string
	DailyLimit       int
	TokenConcurrency int
	MaxRetries       int
	QueueTimeout     time.Duration
	UpstreamTimeout  time.Duration
	MaxSide          int
	DefaultSize      string
	QualitySteps     map[string]int
	DefaultNegative  string
	PublicBaseURL    string
	ImageTTL         time.Duration
	LogRetentionDays int
}

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k != "" && os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

func Load() *Config {
	loadDotEnv(".env")
	def := env("DEFAULT_MODEL", "z-image-turbo")
	c := &Config{
		Addr:             env("ADDR", ":8080"),
		DataDir:          env("DATA_DIR", "./data"),
		AdminPassword:    env("ADMIN_PASSWORD", "admin123"),
		APIKeys:          splitList(env("API_KEYS", "")),
		UpstreamBase:     strings.TrimRight(env("UPSTREAM_BASE", "https://ai.gitee.com/v1"), "/"),
		DefaultModel:     def,
		EditModel:        env("EDIT_MODEL", "Qwen-Image-Edit"),
		DailyLimit:       envInt("TOKEN_DAILY_LIMIT", 100),
		TokenConcurrency: envInt("TOKEN_CONCURRENCY", 2),
		MaxRetries:       envInt("MAX_RETRIES", 3),
		QueueTimeout:     time.Duration(envInt("QUEUE_TIMEOUT_SEC", 60)) * time.Second,
		UpstreamTimeout:  time.Duration(envInt("UPSTREAM_TIMEOUT_SEC", 180)) * time.Second,
		MaxSide:          envInt("MAX_SIDE", 2048),
		DefaultSize:      env("DEFAULT_SIZE", "1024x1024"),
		DefaultNegative:  env("DEFAULT_NEGATIVE_PROMPT", ""),
		PublicBaseURL:    strings.TrimRight(env("PUBLIC_BASE_URL", ""), "/"),
		ImageTTL:         time.Duration(envInt("IMAGE_TTL_HOURS", 24)) * time.Hour,
		LogRetentionDays: envInt("LOG_RETENTION_DAYS", 30),
	}
	c.ModelMap = map[string]string{
		"gpt-image-2":      def,
		"gpt-image-1.5":    def,
		"gpt-image-1":      def,
		"gpt-image-1-mini": def,
		"dall-e-3":         def,
		"dall-e-2":         def,
	}
	for _, kv := range splitList(env("MODEL_MAP", "")) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			c.ModelMap[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	c.QualitySteps = map[string]int{"low": 6, "medium": 9, "high": 12}
	for _, kv := range splitList(env("QUALITY_STEPS", "")) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				c.QualitySteps[strings.TrimSpace(k)] = n
			}
		}
	}
	return c
}

func env(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(k))); err == nil {
		return v
	}
	return d
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ';' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
