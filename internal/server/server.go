// Package server 提供进程内的设置页与同步接口。
//
// 设计要点：
//   - 监听器常驻（即使同步被停用），否则用户关掉服务后就再也打不开设置页；
//     "服务开关"只控制 /api/clipboard 是否对外服务。
//   - 同一端口同时接受明文与 TLS：设置页走 http://127.0.0.1，手机端走 https。
//   - /api/clipboard 只接受设备令牌（Bearer）；设置/事件接口只对 127.0.0.1 开放，
//     并要求一次性本地令牌 + 同源校验，防止局域网内其它主机改配置。
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"cyime-sync/internal/config"
	"cyime-sync/internal/pairing"
	"cyime-sync/internal/syncer"
)

// Options 是 Manager 的依赖注入。
type Options struct {
	Config  *config.Store
	Engine  *syncer.Engine
	Version string
	// Pairing 配对（自签证书 + 逐设备令牌）。必填：本版本不再支持无 TLS 的明文对接。
	Pairing *pairing.Manager
	// ApplyAutoStart 由 main 注入（写 HKCU Run 注册表）。
	ApplyAutoStart func(bool) error
	// Logf 可选日志钩子，默认写标准错误。
	Logf func(format string, args ...any)
}

// Manager 负责监听器生命周期与路由分发。
type Manager struct {
	opts    Options
	token   string
	started time.Time
	logf    func(string, ...any)

	mu      sync.Mutex
	srv     *http.Server
	ln      net.Listener
	addr    string
	lastErr string

	// pairMu/pairSubs 用于把配对事件推给设置页（SSE），避免前端轮询。
	pairMu   sync.Mutex
	pairSubs map[chan string]struct{}

	// applyFn 默认指向 Apply；测试可替换以免真的绑定端口。
	applyFn func() error
}

// New 创建服务管理器并生成本地一次性令牌。
func New(opts Options) (*Manager, error) {
	if opts.Pairing == nil {
		return nil, errors.New("配对模块未初始化：本版本只支持扫码配对")
	}
	tok, err := newToken()
	if err != nil {
		return nil, err
	}
	logf := opts.Logf
	if logf == nil {
		logf = log.Printf
	}
	m := &Manager{
		opts:     opts,
		token:    tok,
		started:  time.Now(),
		logf:     logf,
		pairSubs: make(map[chan string]struct{}),
	}
	m.applyFn = m.Apply
	opts.Pairing.SetNotifier(m.broadcastPairing)
	return m, nil
}

// broadcastPairing 把配对事件推给所有设置页连接。
func (m *Manager) broadcastPairing(event, detail string) {
	m.pairMu.Lock()
	defer m.pairMu.Unlock()
	for ch := range m.pairSubs {
		select {
		case ch <- event + "\x00" + detail:
		default: // 订阅者慢就丢，状态可从 /api/pairing 重新读取
		}
	}
}

// subscribePairing 注册一个配对事件订阅。
func (m *Manager) subscribePairing() (chan string, func()) {
	ch := make(chan string, 8)
	m.pairMu.Lock()
	m.pairSubs[ch] = struct{}{}
	m.pairMu.Unlock()
	return ch, func() {
		m.pairMu.Lock()
		if _, ok := m.pairSubs[ch]; ok {
			delete(m.pairSubs, ch)
			close(ch)
		}
		m.pairMu.Unlock()
	}
}

// Start 按当前配置启动监听。
func (m *Manager) Start() error { return m.Apply() }

// Close 关闭监听器。
func (m *Manager) Close() error {
	m.mu.Lock()
	srv := m.srv
	m.srv, m.ln = nil, nil
	m.mu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// Addr 返回当前监听地址（未监听时为空）。
func (m *Manager) Addr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.addr
}

// LastError 返回最近一次监听失败原因。
func (m *Manager) LastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

// Apply 依据配置重建监听器（端口/地址变化时）。
func (m *Manager) Apply() error {
	cfg := m.opts.Config.Get()
	want := cfg.Listen()

	m.opts.Engine.SetEnabled(cfg.Enabled)

	m.mu.Lock()
	if m.addr == want && m.srv != nil {
		m.mu.Unlock()
		return nil
	}
	old := m.srv
	m.srv, m.ln = nil, nil
	m.mu.Unlock()

	if old != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = old.Shutdown(ctx)
		cancel()
	}

	ln, err := net.Listen("tcp", want)
	if err != nil {
		m.mu.Lock()
		m.lastErr = err.Error()
		m.addr = ""
		m.mu.Unlock()
		m.logf("监听 %s 失败: %v", want, err)
		return fmt.Errorf("监听 %s 失败: %w", want, err)
	}
	tlsConf := m.opts.Pairing.TLSConfig()
	// 同一端口：设置页走明文 HTTP，手机端走 HTTPS。
	ln = &sniffListener{Listener: ln, conf: tlsConf}

	srv := &http.Server{
		Handler:           m.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// 不设置 Server 头细节（net/http 默认不输出）。
	}

	m.mu.Lock()
	m.srv, m.ln = srv, ln
	m.addr = want
	m.lastErr = ""
	m.mu.Unlock()

	m.logf("设置页与同步接口监听于 %s（明文 HTTP 与 HTTPS 共用）", displayAddr(want))
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			m.logf("HTTP 服务退出: %v", serveErr)
		}
	}()
	return nil
}

// scheduleApply 在响应写回之后异步重建监听，避免把当前请求的连接掐断。
func (m *Manager) scheduleApply() {
	go func() {
		time.Sleep(200 * time.Millisecond)
		if err := m.applyFn(); err != nil {
			m.logf("应用新监听设置失败: %v", err)
		}
	}()
}

// Handler 组装路由。
func (m *Manager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", m.handlePage)
	mux.HandleFunc("/md3.css", m.serveAsset("md3.css", "text/css; charset=utf-8"))
	mux.HandleFunc("/app.js", m.serveAsset("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("/icon-64.png", m.serveAsset("icon-64.png", "image/png"))
	mux.HandleFunc("/icon-180.png", m.serveAsset("icon-180.png", "image/png"))
	mux.HandleFunc("/favicon.ico", m.serveAsset("icon-64.png", "image/png"))
	mux.HandleFunc("/api/clipboard", m.handleClipboard)
	mux.HandleFunc("/api/pair", m.handlePair)
	mux.HandleFunc("/api/pairing", m.handlePairSession)
	mux.HandleFunc("/api/devices", m.handleDevices)
	mux.HandleFunc("/api/settings", m.handleSettings)
	mux.HandleFunc("/api/status", m.handleStatus)
	mux.HandleFunc("/api/qr.png", m.handleQR)
	mux.HandleFunc("/api/events", m.handleEvents)
	return noStore(mux)
}

// noStore 为所有响应加上禁用缓存头。
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// newToken 生成 32 位十六进制本地令牌（每次进程启动变化）。
func newToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成本地令牌失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// isLoopback 判断请求是否来自本机。设置类接口只对本机开放。
func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// localAuthed 校验"来自本机 + 携带正确令牌"。
func (m *Manager) localAuthed(r *http.Request) bool {
	if !isLoopback(r) {
		return false
	}
	tok := r.Header.Get("X-Cyime-Token")
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	return constantTimeEqual(tok, m.token)
}

// sameOrigin 校验 Origin/Referer（若存在）是本机同源，防 CSRF。
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		// 非浏览器发起的本地调用（curl）没有 Origin，令牌已经足够。
		return true
	}
	for _, prefix := range []string{"http://127.0.0.1", "http://localhost", "http://[::1]"} {
		if strings.HasPrefix(origin, prefix) {
			return true
		}
	}
	// 也允许与当前 Host 完全一致（例如用户按本机 IP 打开设置页）。
	if strings.Contains(origin, "://"+r.Host) {
		return true
	}
	return false
}

func displayAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "0.0.0.0" || host == "::" {
		if local := firstLocalIPv4(); local != "" {
			return net.JoinHostPort(local, port)
		}
	}
	return addr
}
