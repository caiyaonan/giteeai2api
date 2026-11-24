package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"giteeai2api/internal/config"
	"giteeai2api/internal/pool"
)

type Client struct {
	cfg *config.Config
	hc  *http.Client
}

func New(cfg *config.Config) *Client {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        1024,
		MaxIdleConnsPerHost: 512,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &Client{cfg: cfg, hc: &http.Client{Transport: tr, Timeout: cfg.UpstreamTimeout}}
}

// Error 上游错误，Out 决定令牌如何处置以及是否换令牌重试
type Error struct {
	Status int
	Msg    string
	Out    pool.Outcome
}

func (e *Error) Error() string   { return fmt.Sprintf("upstream %d: %s", e.Status, e.Msg) }
func (e *Error) Retryable() bool { return e.Out != pool.OutClient }

type GenParams struct {
	Model    string
	Prompt   string
	Negative string
	Width    int
	Height   int
	Steps    int
	Guidance *float64
	Seed     *int64
}

func (g GenParams) Body() []byte {
	m := map[string]any{
		"model":           g.Model,
		"prompt":          g.Prompt,
		"width":           g.Width,
		"height":          g.Height,
		"response_format": "b64_json",
	}
	if g.Steps > 0 {
		m["num_inference_steps"] = g.Steps
	}
	if g.Negative != "" {
		m["negative_prompt"] = g.Negative
	}
	if g.Guidance != nil {
		m["guidance_scale"] = *g.Guidance
	}
	if g.Seed != nil {
		m["seed"] = *g.Seed
	}
	b, _ := json.Marshal(m)
	return b
}

func (u *Client) Post(ctx context.Context, token, path, ctype string, body []byte) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u.cfg.UpstreamBase+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Failover-Enabled", "true")
	resp, err := u.hc.Do(req)
	if err != nil {
		return nil, netErr(ctx, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, netErr(ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classify(resp.StatusCode, raw)
	}
	var data struct {
		Data []struct {
			B64 string `json:"b64_json"`
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &data); err != nil || len(data.Data) == 0 {
		return nil, &Error{Status: 502, Msg: "upstream returned no image: " + truncate(string(raw), 200), Out: pool.OutTransient}
	}
	d := data.Data[0]
	if d.B64 != "" {
		img, err := base64.StdEncoding.DecodeString(d.B64)
		if err != nil {
			return nil, &Error{Status: 502, Msg: "invalid base64 from upstream", Out: pool.OutTransient}
		}
		return img, nil
	}
	if d.URL != "" {
		return u.Download(ctx, d.URL)
	}
	return nil, &Error{Status: 502, Msg: "upstream returned neither b64_json nor url", Out: pool.OutTransient}
}

func (u *Client) Download(ctx context.Context, url string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := u.hc.Do(req)
	if err != nil {
		return nil, netErr(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Status: 502, Msg: fmt.Sprintf("download image failed: %d", resp.StatusCode), Out: pool.OutTransient}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, netErr(ctx, err)
	}
	return b, nil
}

func (u *Client) Check(ctx context.Context, token string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.cfg.UpstreamBase+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := u.hc.Do(req)
	if err != nil {
		return netErr(ctx, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return classify(resp.StatusCode, raw)
}

func netErr(ctx context.Context, err error) *Error {
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return &Error{Status: 499, Msg: "client canceled", Out: pool.OutClient}
	}
	return &Error{Status: 502, Msg: err.Error(), Out: pool.OutTransient}
}

var quotaKeywords = []string{"quota", "额度", "余额", "insufficient", "balance", "次数", "daily", "每日", "今日", "用尽", "exhausted", "arrears", "欠费"}

func classify(status int, raw []byte) *Error {
	msg := extractMsg(raw)
	lower := strings.ToLower(msg)
	isQuota := false
	for _, k := range quotaKeywords {
		if strings.Contains(lower, k) {
			isQuota = true
			break
		}
	}
	e := &Error{Status: status, Msg: msg}
	switch {
	case status == 401:
		e.Out = pool.OutInvalid
	case status == 402 || (isQuota && (status == 403 || status == 429)):
		e.Out = pool.OutQuota
	case status == 429:
		e.Out = pool.OutRate
	case status == 403 || status >= 500:
		e.Out = pool.OutTransient
	default:
		e.Out = pool.OutClient
	}
	return e
}

func extractMsg(raw []byte) string {
	var v struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(raw, &v) == nil {
		var inner struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(v.Error, &inner) == nil && inner.Message != "" {
			return inner.Message
		}
		var s string
		if json.Unmarshal(v.Error, &s) == nil && s != "" {
			return s
		}
		if v.Message != "" {
			return v.Message
		}
	}
	return truncate(strings.TrimSpace(string(raw)), 300)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
