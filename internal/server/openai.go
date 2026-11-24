package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"giteeai2api/internal/pool"
	"giteeai2api/internal/store"
	"giteeai2api/internal/upstream"
)

type ImageReq struct {
	Model             string            `json:"model"`
	Prompt            string            `json:"prompt"`
	N                 int               `json:"n"`
	Size              string            `json:"size"`
	Quality           string            `json:"quality"`
	ResponseFormat    string            `json:"response_format"`
	OutputFormat      string            `json:"output_format"`
	OutputCompression *int              `json:"output_compression"`
	Background        string            `json:"background"`
	Stream            bool              `json:"stream"`
	PartialImages     int               `json:"partial_images"`
	Moderation        string            `json:"moderation"`
	Style             string            `json:"style"`
	User              string            `json:"user"`
	NegativePrompt    string            `json:"negative_prompt"`
	Steps             int               `json:"num_inference_steps"`
	GuidanceScale     *float64          `json:"guidance_scale"`
	Seed              *int64            `json:"seed"`
	Images            []json.RawMessage `json:"images"`
	Mask              json.RawMessage   `json:"mask"`

	width, height int
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, typ, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"message": msg, "type": typ, "param": nil, "code": nilIfEmpty(code),
	}})
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func errInfo(err error) (status int, typ, code, msg string) {
	var ue *upstream.Error
	switch {
	case errors.Is(err, pool.ErrNoToken):
		return 503, "server_error", "no_available_token", "No available upstream token, please try again later."
	case errors.Is(err, pool.ErrBusy):
		return 429, "rate_limit_error", "rate_limit_exceeded", "Server is busy, please retry later."
	case errors.As(err, &ue):
		if ue.Out == pool.OutClient {
			st := ue.Status
			if st < 400 || st >= 500 {
				st = 400
			}
			return st, "invalid_request_error", "upstream_rejected", ue.Msg
		}
		return 502, "server_error", "upstream_error", ue.Msg
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return 499, "server_error", "canceled", err.Error()
	}
	return 500, "server_error", "", err.Error()
}

func (s *Server) normalize(r *ImageReq) error {
	r.Prompt = strings.TrimSpace(r.Prompt)
	if r.Prompt == "" {
		return errors.New("'prompt' is required")
	}
	if len([]rune(r.Prompt)) > 32000 {
		return errors.New("'prompt' is too long, maximum is 32000 characters")
	}
	if r.Model == "" {
		r.Model = "gpt-image-2"
	}
	if r.N == 0 {
		r.N = 1
	}
	if r.N < 1 || r.N > 10 {
		return errors.New("'n' must be between 1 and 10")
	}
	switch r.Quality {
	case "", "auto", "standard":
		r.Quality = "medium"
	case "hd":
		r.Quality = "high"
	case "low", "medium", "high":
	default:
		return fmt.Errorf("invalid 'quality': %q", r.Quality)
	}
	if r.ResponseFormat == "" {
		r.ResponseFormat = "b64_json"
	}
	if r.ResponseFormat != "b64_json" && r.ResponseFormat != "url" {
		return fmt.Errorf("invalid 'response_format': %q", r.ResponseFormat)
	}
	switch r.OutputFormat {
	case "":
		r.OutputFormat = "png"
	case "jpg":
		r.OutputFormat = "jpeg"
	case "png", "jpeg", "webp":
	default:
		return fmt.Errorf("invalid 'output_format': %q", r.OutputFormat)
	}
	if r.Background == "" || r.Background == "auto" || r.Background == "transparent" {
		r.Background = "opaque"
	}
	if r.Steps <= 0 {
		r.Steps = s.cfg.QualitySteps[r.Quality]
	}
	if r.NegativePrompt == "" {
		r.NegativePrompt = s.cfg.DefaultNegative
	}
	var err error
	r.width, r.height, err = s.parseSize(r.Size)
	return err
}

func (s *Server) parseSize(raw string) (int, int, error) {
	str := strings.ToLower(strings.TrimSpace(raw))
	if str == "" || str == "auto" {
		str = s.cfg.DefaultSize
	}
	str = strings.NewReplacer("*", "x", "×", "x", " ", "").Replace(str)
	ws, hs, ok := strings.Cut(str, "x")
	w, e1 := strconv.Atoi(ws)
	h, e2 := strconv.Atoi(hs)
	if !ok || e1 != nil || e2 != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("invalid 'size': %q, expected format like 1024x1024 or auto", str)
	}
	if r := float64(max(w, h)) / float64(min(w, h)); r > 3 {
		return 0, 0, errors.New("invalid 'size': aspect ratio must not exceed 3:1")
	}
	if m := max(w, h); m > s.cfg.MaxSide {
		w, h = w*s.cfg.MaxSide/m, h*s.cfg.MaxSide/m
	}
	align := func(v int) int { return max((v+8)/16*16, 256) }
	return align(w), align(h), nil
}

func (s *Server) upstreamModel(m string, edit bool) string {
	if mapped, ok := s.cfg.ModelMap[m]; ok {
		if edit {
			return s.cfg.EditModel
		}
		return mapped
	}
	return m
}

func estimateUsage(r *ImageReq, images, inputImages int) map[string]any {
	text := (len(r.Prompt) + 3) / 4
	imgIn := inputImages * 323
	per := map[string]float64{"low": 272, "medium": 1056, "high": 4160}[r.Quality]
	out := int(per*float64(r.width*r.height)/(1024*1024)) * images
	return map[string]any{
		"input_tokens":         text + imgIn,
		"input_tokens_details": map[string]int{"text_tokens": text, "image_tokens": imgIn},
		"output_tokens":        out,
		"total_tokens":         text + imgIn + out,
	}
}

type callFn func(ctx context.Context, token string) ([]byte, error)

func (s *Server) withToken(ctx context.Context, call callFn) (img []byte, tokenID int64, attempts int, err error) {
	tried := map[int64]bool{}
	for attempts < s.cfg.MaxRetries+1 {
		t, e := s.pool.Acquire(ctx, tried)
		if e != nil {
			if err == nil {
				err = e
			}
			return
		}
		attempts++
		tokenID = t.ID
		b, e := call(ctx, t.Value)
		if e == nil {
			s.pool.Release(t, pool.OutOK, "")
			return b, tokenID, attempts, nil
		}
		var ue *upstream.Error
		if !errors.As(e, &ue) {
			ue = &upstream.Error{Status: 502, Msg: e.Error(), Out: pool.OutTransient}
		}
		s.pool.Release(t, ue.Out, ue.Msg)
		err = ue
		if !ue.Retryable() {
			return
		}
		tried[t.ID] = true
	}
	return
}

type logMeta struct {
	keyID    int64
	endpoint string
	model    string
	upstream string
	size     string
}

type result struct {
	data   []byte
	format string
	err    error
}

func (s *Server) runJobs(ctx context.Context, r *ImageReq, meta logMeta, call callFn, onDone func(int, result)) []result {
	res := make([]result, r.N)
	var wg sync.WaitGroup
	for i := 0; i < r.N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start := time.Now()
			img, tokenID, attempts, err := s.withToken(ctx, call)
			rs := result{err: err}
			if err == nil {
				rs.data, rs.format = convertImage(img, r.OutputFormat, r.OutputCompression)
			}
			s.logReq(meta, tokenID, attempts, time.Since(start), err)
			res[i] = rs
			if onDone != nil {
				onDone(i, rs)
			}
		}(i)
	}
	wg.Wait()
	return res
}

func (s *Server) logReq(m logMeta, tokenID int64, attempts int, d time.Duration, err error) {
	status, errMsg := 200, ""
	if err != nil {
		status, _, _, errMsg = errInfo(err)
	}
	ts := time.Now().Unix()
	s.store.Async(func(tx *sql.Tx) {
		store.ExecTx(tx, `INSERT INTO logs(ts,key_id,token_id,endpoint,model,upstream_model,size,status,latency_ms,attempts,error)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, ts, m.keyID, tokenID, m.endpoint, m.model, m.upstream, m.size,
			status, d.Milliseconds(), attempts, truncate(errMsg, 500))
	})
}

func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.PublicBaseURL != "" {
		return s.cfg.PublicBaseURL
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

func (s *Server) imageItem(hr *http.Request, r *ImageReq, rs result) (map[string]any, error) {
	item := map[string]any{}
	if r.ResponseFormat == "url" {
		b := make([]byte, 16)
		rand.Read(b)
		name := hex.EncodeToString(b) + "." + extOf(rs.format)
		if err := os.WriteFile(filepath.Join(s.cfg.DataDir, "images", name), rs.data, 0o644); err != nil {
			return nil, err
		}
		item["url"] = s.baseURL(hr) + "/images/" + name
	} else {
		item["b64_json"] = base64.StdEncoding.EncodeToString(rs.data)
	}
	if r.Model == "dall-e-3" {
		item["revised_prompt"] = r.Prompt
	}
	return item, nil
}

func (s *Server) respond(w http.ResponseWriter, hr *http.Request, r *ImageReq, results []result, inputImages int) {
	var items []map[string]any
	var firstErr error
	format := r.OutputFormat
	for _, rs := range results {
		if rs.err != nil {
			if firstErr == nil {
				firstErr = rs.err
			}
			continue
		}
		item, err := s.imageItem(hr, r, rs)
		if err != nil {
			firstErr = err
			continue
		}
		format = rs.format
		items = append(items, item)
	}
	if len(items) == 0 {
		st, typ, code, msg := errInfo(firstErr)
		writeErr(w, st, typ, code, msg)
		return
	}
	writeJSON(w, 200, map[string]any{
		"created":       time.Now().Unix(),
		"data":          items,
		"background":    r.Background,
		"output_format": format,
		"quality":       r.Quality,
		"size":          fmt.Sprintf("%dx%d", r.width, r.height),
		"usage":         estimateUsage(r, len(items), inputImages),
	})
}

func (s *Server) stream(w http.ResponseWriter, r *ImageReq, prefix string, inputImages int, run func(onDone func(int, result))) {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	var mu sync.Mutex
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		mu.Lock()
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if fl != nil {
			fl.Flush()
		}
		mu.Unlock()
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				mu.Lock()
				io.WriteString(w, ": ping\n\n")
				if fl != nil {
					fl.Flush()
				}
				mu.Unlock()
			}
		}
	}()
	run(func(i int, rs result) {
		if rs.err != nil {
			_, typ, code, msg := errInfo(rs.err)
			send("error", map[string]any{"type": "error", "error": map[string]any{"type": typ, "code": code, "message": msg}})
			return
		}
		ev := prefix + ".completed"
		send(ev, map[string]any{
			"type":          ev,
			"b64_json":      base64.StdEncoding.EncodeToString(rs.data),
			"created_at":    time.Now().Unix(),
			"size":          fmt.Sprintf("%dx%d", r.width, r.height),
			"quality":       r.Quality,
			"background":    r.Background,
			"output_format": rs.format,
			"usage":         estimateUsage(r, 1, inputImages),
		})
	})
	close(done)
}

func (s *Server) handleGenerations(w http.ResponseWriter, hr *http.Request) {
	keyID, ok := s.keys.Auth(hr)
	if !ok {
		writeErr(w, 401, "invalid_request_error", "invalid_api_key", "Incorrect API key provided.")
		return
	}
	var r ImageReq
	if err := json.NewDecoder(io.LimitReader(hr.Body, 1<<20)).Decode(&r); err != nil {
		writeErr(w, 400, "invalid_request_error", "invalid_json", "Invalid JSON body: "+err.Error())
		return
	}
	if err := s.normalize(&r); err != nil {
		writeErr(w, 400, "invalid_request_error", "invalid_value", err.Error())
		return
	}
	upModel := s.upstreamModel(r.Model, false)
	body := upstream.GenParams{Model: upModel, Prompt: r.Prompt, Negative: r.NegativePrompt, Width: r.width, Height: r.height,
		Steps: r.Steps, Guidance: r.GuidanceScale, Seed: r.Seed}.Body()
	call := func(ctx context.Context, token string) ([]byte, error) {
		return s.up.Post(ctx, token, "/images/generations", "application/json", body)
	}
	meta := logMeta{keyID: keyID, endpoint: "generations", model: r.Model, upstream: upModel, size: fmt.Sprintf("%dx%d", r.width, r.height)}
	if r.Stream {
		s.stream(w, &r, "image_generation", 0, func(onDone func(int, result)) { s.runJobs(hr.Context(), &r, meta, call, onDone) })
		return
	}
	s.respond(w, hr, &r, s.runJobs(hr.Context(), &r, meta, call, nil), 0)
}

type filePart struct {
	name  string
	ctype string
	data  []byte
}

func (s *Server) handleEdits(w http.ResponseWriter, hr *http.Request) {
	keyID, ok := s.keys.Auth(hr)
	if !ok {
		writeErr(w, 401, "invalid_request_error", "invalid_api_key", "Incorrect API key provided.")
		return
	}
	var r ImageReq
	var images []filePart
	var mask *filePart
	var err error
	if strings.HasPrefix(hr.Header.Get("Content-Type"), "multipart/") {
		images, mask, err = parseMultipartEdit(hr, &r)
	} else {
		images, mask, err = s.parseJSONEdit(hr, &r)
	}
	if err != nil {
		writeErr(w, 400, "invalid_request_error", "invalid_value", err.Error())
		return
	}
	if len(images) == 0 {
		writeErr(w, 400, "invalid_request_error", "invalid_value", "'image' is required")
		return
	}
	if len(images) > 16 {
		writeErr(w, 400, "invalid_request_error", "invalid_value", "too many images, maximum is 16")
		return
	}
	sizeGiven := r.Size != "" && r.Size != "auto"
	if err := s.normalize(&r); err != nil {
		writeErr(w, 400, "invalid_request_error", "invalid_value", err.Error())
		return
	}
	upModel := s.upstreamModel(r.Model, true)
	fields := map[string]string{"model": upModel, "prompt": r.Prompt, "response_format": "b64_json"}
	if sizeGiven {
		fields["size"] = fmt.Sprintf("%dx%d", r.width, r.height)
		fields["width"], fields["height"] = strconv.Itoa(r.width), strconv.Itoa(r.height)
	}
	if r.NegativePrompt != "" {
		fields["negative_prompt"] = r.NegativePrompt
	}
	if r.Steps > 0 {
		fields["num_inference_steps"] = strconv.Itoa(r.Steps)
	}
	if r.GuidanceScale != nil {
		fields["guidance_scale"] = strconv.FormatFloat(*r.GuidanceScale, 'f', -1, 64)
	}
	if r.Seed != nil {
		fields["seed"] = strconv.FormatInt(*r.Seed, 10)
	}
	body, ctype := buildMultipart(fields, images, mask)
	call := func(ctx context.Context, token string) ([]byte, error) {
		return s.up.Post(ctx, token, "/images/edits", ctype, body)
	}
	meta := logMeta{keyID: keyID, endpoint: "edits", model: r.Model, upstream: upModel, size: fmt.Sprintf("%dx%d", r.width, r.height)}
	if r.Stream {
		s.stream(w, &r, "image_edit", len(images), func(onDone func(int, result)) { s.runJobs(hr.Context(), &r, meta, call, onDone) })
		return
	}
	s.respond(w, hr, &r, s.runJobs(hr.Context(), &r, meta, call, nil), len(images))
}

func parseMultipartEdit(hr *http.Request, r *ImageReq) ([]filePart, *filePart, error) {
	if err := hr.ParseMultipartForm(64 << 20); err != nil {
		return nil, nil, fmt.Errorf("invalid multipart body: %v", err)
	}
	f := hr.MultipartForm
	get := func(k string) string {
		if v := f.Value[k]; len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}
	r.Model, r.Prompt, r.Size, r.Quality = get("model"), get("prompt"), get("size"), get("quality")
	r.ResponseFormat, r.OutputFormat, r.Background = get("response_format"), get("output_format"), get("background")
	r.NegativePrompt = get("negative_prompt")
	r.N, _ = strconv.Atoi(get("n"))
	r.Steps, _ = strconv.Atoi(get("num_inference_steps"))
	r.Stream = get("stream") == "true"
	if v, err := strconv.Atoi(get("output_compression")); err == nil {
		r.OutputCompression = &v
	}
	if v, err := strconv.ParseFloat(get("guidance_scale"), 64); err == nil {
		r.GuidanceScale = &v
	}
	if v, err := strconv.ParseInt(get("seed"), 10, 64); err == nil {
		r.Seed = &v
	}
	read := func(fh *multipart.FileHeader) (filePart, error) {
		file, err := fh.Open()
		if err != nil {
			return filePart{}, err
		}
		defer file.Close()
		b, err := io.ReadAll(file)
		return filePart{name: fh.Filename, ctype: http.DetectContentType(b), data: b}, err
	}
	var images []filePart
	for _, k := range []string{"image", "image[]"} {
		for _, fh := range f.File[k] {
			p, err := read(fh)
			if err != nil {
				return nil, nil, err
			}
			images = append(images, p)
		}
	}
	var mask *filePart
	if fhs := f.File["mask"]; len(fhs) > 0 {
		p, err := read(fhs[0])
		if err != nil {
			return nil, nil, err
		}
		mask = &p
	}
	return images, mask, nil
}

func (s *Server) parseJSONEdit(hr *http.Request, r *ImageReq) ([]filePart, *filePart, error) {
	if err := json.NewDecoder(io.LimitReader(hr.Body, 80<<20)).Decode(r); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON body: %v", err)
	}
	var images []filePart
	for i, raw := range r.Images {
		p, err := s.loadImageRef(hr.Context(), raw, fmt.Sprintf("image_%d", i))
		if err != nil {
			return nil, nil, err
		}
		images = append(images, p)
	}
	var mask *filePart
	if len(r.Mask) > 0 && string(r.Mask) != "null" {
		p, err := s.loadImageRef(hr.Context(), r.Mask, "mask")
		if err != nil {
			return nil, nil, err
		}
		mask = &p
	}
	return images, mask, nil
}

func (s *Server) loadImageRef(ctx context.Context, raw json.RawMessage, name string) (filePart, error) {
	var ref string
	if json.Unmarshal(raw, &ref) != nil {
		var obj struct {
			ImageURL string `json:"image_url"`
			URL      string `json:"url"`
			FileID   string `json:"file_id"`
		}
		json.Unmarshal(raw, &obj)
		if obj.FileID != "" {
			return filePart{}, errors.New("file_id is not supported, use image_url (data URL or http URL)")
		}
		ref = obj.ImageURL + obj.URL
	}
	var b []byte
	var err error
	switch {
	case strings.HasPrefix(ref, "data:"):
		_, enc, _ := strings.Cut(ref, ",")
		b, err = base64.StdEncoding.DecodeString(enc)
	case strings.HasPrefix(ref, "http://"), strings.HasPrefix(ref, "https://"):
		b, err = s.up.Download(ctx, ref)
	case ref != "":
		b, err = base64.StdEncoding.DecodeString(ref)
	default:
		err = errors.New("empty image reference")
	}
	if err != nil {
		return filePart{}, fmt.Errorf("invalid image %s: %v", name, err)
	}
	ct := http.DetectContentType(b)
	return filePart{name: name + "." + extOf(detectFormat(b)), ctype: ct, data: b}, nil
}

func buildMultipart(fields map[string]string, images []filePart, mask *filePart) ([]byte, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	add := func(field string, p filePart) {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, field, strings.ReplaceAll(p.name, `"`, "")))
		h.Set("Content-Type", p.ctype)
		pw, _ := mw.CreatePart(h)
		pw.Write(p.data)
	}
	for _, p := range images {
		add("image", p)
	}
	if mask != nil {
		add("mask", *mask)
	}
	mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

func (s *Server) handleModels(w http.ResponseWriter, hr *http.Request) {
	if _, ok := s.keys.Auth(hr); !ok {
		writeErr(w, 401, "invalid_request_error", "invalid_api_key", "Incorrect API key provided.")
		return
	}
	var data []map[string]any
	for id := range s.cfg.ModelMap {
		data = append(data, map[string]any{"id": id, "object": "model", "created": 1700000000, "owned_by": "openai"})
	}
	data = append(data, map[string]any{"id": s.cfg.DefaultModel, "object": "model", "created": 1700000000, "owned_by": "gitee-ai"})
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}
