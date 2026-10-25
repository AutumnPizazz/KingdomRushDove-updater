// Package gui 是本地图形界面：内置 HTTP 服务加一张浏览器页面，没有外部依赖。
// 不用 Fyne、Wails 这类桌面框架，是因为它们要 cgo 或额外工具链，
// 与「一条命令交叉编译出单文件 exe」冲突。
package gui

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"krd-updater/internal/assetindex"
	"krd-updater/internal/config"
	"krd-updater/internal/osutil"
	"krd-updater/internal/pipeline"
	"krd-updater/internal/remote"
	"krd-updater/internal/ui"
)

//go:embed index.html
var indexHTML []byte

// Line 是一条日志。
type Line struct {
	Seq  int    `json:"seq"`
	Text string `json:"text"`
}

// Hub 收集日志行供页面轮询，本身实现 io.Writer。
type Hub struct {
	mu    sync.Mutex
	lines []Line
	seq   int
	buf   strings.Builder
	max   int
}

// NewHub 默认保留最近 3000 行。
func NewHub() *Hub { return &Hub{max: 3000} }

func (h *Hub) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf.Write(p)
	for {
		s := h.buf.String()
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		h.pushLocked(s[:i])
		h.buf.Reset()
		h.buf.WriteString(s[i+1:])
	}
	return len(p), nil
}

func (h *Hub) pushLocked(s string) {
	h.seq++
	s = strings.TrimRight(s, "\r")
	if strings.TrimSpace(s) != "" {
		h.lines = append(h.lines, Line{Seq: h.seq, Text: s})
		if len(h.lines) > h.max {
			h.lines = h.lines[len(h.lines)-h.max:]
		}
	}
}

// Since 返回 seq 大于 after 的日志行。
func (h *Hub) Since(after int) ([]Line, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Line, 0, 64)
	for _, ln := range h.lines {
		if ln.Seq > after {
			out = append(out, ln)
		}
	}
	return out, h.seq
}

// Server 是图形界面的后端。
type Server struct {
	cfg     config.Config
	cfgPath string
	hub     *Hub
	dirs    []string

	mu      sync.Mutex
	running bool
	plan    *pipeline.Plan
	result  *pipeline.Result
	lastErr string

	// 页面存活判断：页面每 15 秒发一次心跳，关掉时发一次 /api/bye。
	pageSeen  bool
	lastSeen  time.Time
	quitOnBye bool
	quitTimer *time.Timer
	quitCh    chan struct{}
	quitOnce  sync.Once
}

// New 用给定配置创建服务端。
func New(cfg config.Config) *Server {
	return &Server{
		cfg:     cfg,
		cfgPath: cfg.ConfigPath,
		hub:     NewHub(),
		dirs:    osutil.InstallCandidates(),
		quitCh:  make(chan struct{}),
	}
}

// Run 启动服务并阻塞到 ctx 结束、页面关闭或点了「退出程序」。
// addr 为空时随机选端口。idleTimeout 大于 0 时，页面心跳停了这么久就退出。
func (s *Server) Run(ctx context.Context, addr string, openBrowser bool, idleTimeout time.Duration) error {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", addr, err)
	}
	url := "http://" + ln.Addr().String() + "/"
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/detect", s.handleDetect)
	mux.HandleFunc("/api/check", s.handleCheck)
	mux.HandleFunc("/api/update", s.handleUpdate)
	mux.HandleFunc("/api/verify", s.handleVerify)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/log", s.handleLog)
	mux.HandleFunc("/api/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/api/bye", s.handleBye)
	mux.HandleFunc("/api/quit", s.handleQuit)

	// 除了 bye 以外的请求都算页面还活着，顺手取消待执行的退出。
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/bye" {
			s.touch()
			s.cancelQuit()
		}
		mux.ServeHTTP(w, r)
	})

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	if idleTimeout > 0 {
		s.mu.Lock()
		s.quitOnBye = true
		s.mu.Unlock()
		go s.watchPage(idleTimeout)
	}

	fmt.Printf("图形界面已启动：%s\n", url)
	fmt.Println("页面里改完配置点「保存配置」即可；关掉页面后程序会自己退出。")
	if openBrowser {
		if err := osutil.OpenBrowser(url); err != nil {
			fmt.Printf("没能自动打开浏览器，请手动访问上面的地址（%v）\n", err)
			osutil.MessageBox("王国保卫战 Dove 版更新器", "浏览器没有打开。请在浏览器里访问：\n\n"+url)
		}
	}

	select {
	case <-ctx.Done():
		s.requestQuit()
	case <-s.quitCh:
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}

	shut, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = srv.Shutdown(shut)
	cancel()
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// touch 记下页面刚刚发过请求。
func (s *Server) touch() {
	s.mu.Lock()
	s.pageSeen = true
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

// requestQuit 关掉 quitCh，Run 收到后收尾退出。多次调用只生效一次。
func (s *Server) requestQuit() {
	s.quitOnce.Do(func() { close(s.quitCh) })
}

// scheduleQuit 安排一次延迟退出。页面刷新时会重新发请求，把它取消掉。
func (s *Server) scheduleQuit(d time.Duration) {
	s.mu.Lock()
	if s.quitTimer != nil {
		s.quitTimer.Stop()
	}
	s.quitTimer = time.AfterFunc(d, s.requestQuit)
	s.mu.Unlock()
}

// cancelQuit 取消还没到点的延迟退出。
func (s *Server) cancelQuit() {
	s.mu.Lock()
	if s.quitTimer != nil {
		s.quitTimer.Stop()
		s.quitTimer = nil
	}
	s.mu.Unlock()
}

// watchPage 在页面心跳停太久时退出，兜住浏览器崩溃、标签页被强制结束这类情况。
func (s *Server) watchPage(idle time.Duration) {
	tick := idle / 4
	if tick < time.Second {
		tick = time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-s.quitCh:
			return
		case <-t.C:
			s.mu.Lock()
			seen, last := s.pageSeen, s.lastSeen
			s.mu.Unlock()
			if !seen {
				continue
			}
			if time.Since(last) > idle {
				fmt.Printf("页面已经 %s 没动静，退出程序。\n", idle)
				s.requestQuit()
				return
			}
		}
	}
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	s.touch()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleBye 由页面关闭时的 sendBeacon 调用。延迟两秒退出，刷新页面则会被取消。
// idle-timeout 设成 0 时只回个 ok，程序留着不退。
func (s *Server) handleBye(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true})
	s.mu.Lock()
	on := s.quitOnBye
	s.mu.Unlock()
	if on {
		s.scheduleQuit(2 * time.Second)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"ok": false, "error": err.Error()})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) snapshot() config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

type configPayload struct {
	Config   *config.Config `json:"config"`
	Path     string         `json:"path"`
	OnlySave bool           `json:"only_save"`
}

// mergeConfig 把界面上的表单值合并进配置并做一次完整校验。
func mergeConfig(base config.Config, in *config.Config) (config.Config, error) {
	if in == nil {
		return base, nil
	}
	cfg := base
	cfg.InstallDir = strings.TrimSpace(in.InstallDir)
	cfg.Sites = in.Sites
	cfg.TimeoutSeconds = in.TimeoutSeconds
	cfg.Retries = in.Retries
	cfg.ParallelDownloads = in.ParallelDownloads
	cfg.SkipAssets = in.SkipAssets
	cfg.DeleteRemoved = in.DeleteRemoved
	cfg.Backup = in.Backup
	cfg.TempDir = in.TempDir
	cfg.KeepTemp = in.KeepTemp
	cfg.LogFile = in.LogFile
	cfg.Proxy = in.Proxy
	cfg.InsecureTLS = in.InsecureTLS
	cfg.Elevate = in.Elevate
	cfg.Progress = in.Progress
	cfg.Verbose = in.Verbose
	return cfg, cfg.Finalize()
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.snapshot()
		localHash, _ := pipeline.ReadLocalHash(cfg)
		writeJSON(w, 200, map[string]any{
			"ok":          true,
			"config":      cfg,
			"config_path": s.cfgPath,
			"candidates":  s.dirs,
			"version":     localVersion(cfg.InstallDir),
			"local_hash":  localHash,
		})
	case http.MethodPost:
		var p configPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			writeErr(w, 400, err)
			return
		}
		base := s.snapshot()
		cfg, err := mergeConfig(base, p.Config)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		path := p.Path
		if path == "" {
			path = s.cfgPath
		}
		if path == "" {
			path = "krd-updater.jsonc"
		}
		if err := config.Save(path, cfg); err != nil {
			writeErr(w, 500, err)
			return
		}
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		s.mu.Lock()
		s.cfg = cfg
		s.cfgPath = path
		s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true, "config_path": path})
	default:
		w.WriteHeader(405)
	}
}

func (s *Server) handleDetect(w http.ResponseWriter, r *http.Request) {
	var found []string
	for _, d := range osutil.InstallCandidates() {
		if osutil.IsInstallDir(d) {
			found = append(found, d)
		}
	}
	if len(found) == 0 {
		if d, err := osutil.DetectInstallDir(); err == nil {
			found = append(found, d)
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "found": found})
}

// prepareCfg 应用表单值、校验目录，并返回可用于联网的配置。
func (s *Server) prepareCfg(p configPayload) (config.Config, error) {
	base := s.snapshot()
	cfg, err := mergeConfig(base, p.Config)
	if err != nil {
		return cfg, err
	}
	if cfg.InstallDir == "" {
		d, derr := osutil.DetectInstallDir()
		if derr != nil {
			return cfg, derr
		}
		cfg.InstallDir = d
	}
	if !osutil.IsInstallDir(cfg.InstallDir) {
		return cfg, fmt.Errorf("这个目录不像游戏安装目录（缺少 main.lua/_assets）: %s", cfg.InstallDir)
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	return cfg, nil
}

func (s *Server) logger(cfg config.Config) (*ui.Logger, error) {
	hubWriter := &teeWriter{a: s.hub, b: os.Stdout}
	return ui.NewLoggerTo(hubWriter, cfg.Verbose, cfg.LogFile, false)
}

// teeWriter 同时写到界面日志与终端。
type teeWriter struct {
	a, b interface {
		Write([]byte) (int, error)
	}
}

func (t *teeWriter) Write(p []byte) (int, error) {
	if t.a != nil {
		if _, err := t.a.Write(p); err != nil {
			return 0, err
		}
	}
	if t.b != nil {
		_, _ = t.b.Write(p)
	}
	return len(p), nil
}

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	var p configPayload
	_ = json.NewDecoder(r.Body).Decode(&p)
	cfg, err := s.prepareCfg(p)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	log, err := s.logger(cfg)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	defer log.Close()
	client, err := remote.New(cfg, log)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	log.Step("检查更新……")
	plan, err := pipeline.Check(r.Context(), cfg, log, client, cfg.SkipAssets)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	s.mu.Lock()
	s.plan = plan
	s.lastErr = ""
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"ok":           true,
		"site":         plan.Site,
		"local_hash":   plan.LocalHash,
		"remote_hash":  plan.RemoteHash,
		"up_to_date":   plan.AlreadyLatest,
		"commits":      plan.Commits,
		"code_files":   len(plan.CodeFiles),
		"asset_files":  len(plan.AssetNeeds),
		"asset_bytes":  plan.AssetTotal,
		"deleted":      len(plan.DeletedFiles),
		"local_assets": plan.LocalAssets,
	})
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var p configPayload
	_ = json.NewDecoder(r.Body).Decode(&p)
	cfg, err := s.prepareCfg(p)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		writeErr(w, 409, errors.New("已经有一个更新任务在跑"))
		return
	}
	plan := s.plan
	if plan == nil || plan.AlreadyLatest {
		s.mu.Unlock()
		writeErr(w, 409, errors.New("请先点「检查更新」"))
		return
	}
	if !osutil.IsElevated() && !osutil.CanWriteTree(cfg.InstallDir) {
		s.mu.Unlock()
		writeErr(w, 403, fmt.Errorf("没有写权限，请右键「以管理员身份运行」本程序后再试: %s", cfg.InstallDir))
		return
	}
	if running, exe := osutil.GameRunning(cfg.InstallDir); running {
		s.mu.Unlock()
		writeErr(w, 409, fmt.Errorf("游戏正在运行（%s），请先关闭游戏", exe))
		return
	}
	s.running = true
	s.result = nil
	s.lastErr = ""
	s.mu.Unlock()

	go func() {
		log, lerr := s.logger(cfg)
		if lerr != nil {
			s.finish(nil, lerr)
			return
		}
		defer log.Close()
		client, cerr := remote.New(cfg, log)
		if cerr != nil {
			s.finish(nil, cerr)
			return
		}
		res, aerr := pipeline.Apply(context.Background(), cfg, log, client, plan, pipeline.Options{
			SkipAssets: cfg.SkipAssets,
			NoDelete:   !cfg.DeleteRemoved,
			Backup:     cfg.Backup.Enabled,
			BackupDir:  cfg.Backup.Dir,
			KeepTemp:   cfg.KeepTemp,
			Verbose:    cfg.Verbose,
		})
		if aerr == nil {
			log.Step("完成")
			log.Info("代码/数据 %d 个，资源 %d 个，删除 %d 个，共写入 %s，用时 %s",
				res.CodeFiles, res.AssetFiles, res.DeletedFiles, ui.HumanBytes(res.Bytes), ui.HumanDuration(res.Elapsed))
			s.mu.Lock()
			s.plan = nil // 更新完强制重新检查
			s.mu.Unlock()
		}
		s.finish(res, aerr)
	}()

	writeJSON(w, 200, map[string]any{"ok": true, "started": true})
}

func (s *Server) finish(res *pipeline.Result, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	s.result = res
	if err != nil {
		s.lastErr = err.Error()
	}
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	var p configPayload
	_ = json.NewDecoder(r.Body).Decode(&p)
	cfg, err := s.prepareCfg(p)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	log, err := s.logger(cfg)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	defer log.Close()
	client, err := remote.New(cfg, log)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	log.Step("校验资源……")
	body, err := client.AssetIndex(r.Context(), cfg.IndexFile)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	idx := assetindex.Parse(body)
	bad, checked := assetindex.Verify(filepath.Join(cfg.InstallDir, "_assets"), idx)
	log.Info("校验 %d 个资源，异常 %d 个", checked, len(bad))
	for i, p := range bad {
		if i >= 30 {
			log.Info("  …… 其余 %d 个省略", len(bad)-i)
			break
		}
		log.Info("  ! %s", p)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "checked": checked, "bad": bad})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{"ok": true, "running": s.running, "error": s.lastErr}
	if s.result != nil {
		out["result"] = map[string]any{
			"code_files":    s.result.CodeFiles,
			"asset_files":   s.result.AssetFiles,
			"deleted_files": s.result.DeletedFiles,
			"bytes":         s.result.Bytes,
			"seconds":       s.result.Elapsed.Seconds(),
			"backup_dir":    s.result.BackupDir,
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	after := 0
	if v := r.URL.Query().Get("after"); v != "" {
		fmt.Sscanf(v, "%d", &after)
	}
	lines, seq := s.hub.Since(after)
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "lines": lines, "seq": seq, "running": running})
}

// handleQuit 对应页面上的「退出程序」按钮。先回响应，再走收尾流程。
func (s *Server) handleQuit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true})
	go func() {
		time.Sleep(200 * time.Millisecond)
		s.requestQuit()
	}()
}

func localVersion(installDir string) string {
	raw, err := os.ReadFile(filepath.Join(installDir, "version.lua"))
	if err != nil {
		return "未知"
	}
	s := string(raw)
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "id") && strings.Contains(t, "=") {
			v := strings.SplitN(t, "=", 2)[1]
			v = strings.TrimSpace(strings.TrimSuffix(v, ","))
			return strings.Trim(v, `"'`)
		}
	}
	return "未知"
}
