// Package relay 实现「电脑主动推给手机」这一侧。
//
// 协议背景：2026-11 起 CyIME 手机端默认不再每 2 秒轮询电脑，而是让每台设备互相推送。
// 手机通过 GET /api/status 看到 `push: true` 后，会 POST /api/peer 把自己的监听端口、
// 证书指纹和一次授权令牌交给电脑；之后电脑在剪贴板变化时直接 PUT 到手机。
//
// 本包只做三件事：盯住引擎缓存里的可推送内容、按设备登记的目标推送、把结果回报给上层
// （成功即「在线」，失败则让状态接口挂上 push_failed，手机据此退回旧版轮询）。
//
// 不做的事：不读剪贴板、不写事件、不管隐私策略——内容能进缓存就说明已经过 syncer 的
// 字符上限与敏感内容判定，这里照发即可。
package relay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cyime-sync/internal/clipboard"
	"cyime-sync/internal/protocol"
	"cyime-sync/internal/syncer"
)

const (
	// TickInterval 检查缓存是否变化的间隔。真正的推送由内容变化触发，
	// 300ms 足够快（手机端旧版轮询是 2 秒），也不会让 tick 变成负担。
	TickInterval = 300 * time.Millisecond
	// pushTimeout 单次推送的总超时。手机端局域网内正常响应在 100ms 量级。
	pushTimeout = 6 * time.Second
)

// retryDelays 首次失败后的重试间隔。三次都失败才回写「推送失败」，
// 避免一次 Wi-Fi 抖动就把手机推回轮询模式。
var retryDelays = []time.Duration{0, 2 * time.Second, 8 * time.Second}

// Target 是一台可接收推送的设备。全部字段都来自手机自己的登记（POST /api/peer）。
type Target struct {
	DeviceID    string
	Host        string
	Port        int
	Fingerprint string // 手机证书叶 DER 的 SHA-256 小写十六进制
	Token       string // 手机发放的 Bearer 令牌（43 字符）
}

// url 返回推送地址。IPv6 地址由 net.JoinHostPort 自动加方括号。
func (t Target) url() string {
	return "https://" + net.JoinHostPort(t.Host, fmt.Sprint(t.Port)) + "/api/clipboard"
}

// Relay 负责把当前可推送内容送到已登记的设备。
type Relay struct {
	engine   *syncer.Engine
	targets  func() []Target
	enabled  func() bool
	onResult func(deviceID string, ok bool, err error)
	logf     func(format string, args ...any)

	mu       sync.Mutex
	lastETag string
	// pushed 记录每台设备最近一次成功推送的内容 hash，内容没变就不重复打扰。
	pushed map[string]string
	// inbound 记录「这条内容是哪台手机推来的」，回推时跳过它：对方剪贴板里已经是这条，
	// 再推一次只会在它又复制了新内容后把旧内容盖回去。
	inbound map[string]string
	order   []string
	// deviceLocks 让同一台设备的推送串行：连打两次复制时，旧内容不能比新内容更晚到达，
	// 否则手机最后停在被覆盖前的旧内容上。
	deviceLocks map[string]*sync.Mutex

	clientMu sync.Mutex
	clients  map[string]*http.Client

	kick      chan struct{}
	stop      chan struct{}
	done      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
	started   atomic.Bool
}

// New 创建推送器。targets 每次推送前重新读取，设备增删无需重启。
func New(engine *syncer.Engine, targets func() []Target, enabled func() bool,
	onResult func(deviceID string, ok bool, err error), logf func(string, ...any)) *Relay {
	if enabled == nil {
		enabled = func() bool { return true }
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Relay{
		engine:      engine,
		targets:     targets,
		enabled:     enabled,
		onResult:    onResult,
		logf:        logf,
		pushed:      make(map[string]string),
		inbound:     make(map[string]string),
		clients:     make(map[string]*http.Client),
		deviceLocks: make(map[string]*sync.Mutex),
		kick:        make(chan struct{}, 1),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
}

// Start 启动后台检查协程。
func (r *Relay) Start() {
	r.startOnce.Do(func() {
		r.started.Store(true)
		go r.loop()
	})
}

// Stop 停止推送并放弃未完成的重试。
func (r *Relay) Stop() {
	r.stopOnce.Do(func() {
		close(r.stop)
		if r.started.Load() {
			<-r.done
		}
	})
}

// NoteInbound 记住这条内容是这台设备推来的，回推时跳过它。
//
// 必须在写缓存之前调用：tick 可能恰好插在「写完缓存」和「登记来源」之间，
// 那条内容就会被原样推回给它。
func (r *Relay) NoteInbound(deviceID, hash string) {
	if deviceID == "" || hash == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.inbound[hash]; !ok {
		r.order = append(r.order, hash)
		if len(r.order) > 8 {
			delete(r.inbound, r.order[0])
			r.order = r.order[1:]
		}
	}
	r.inbound[hash] = deviceID
}

// Kick 立即重新检查一次（设备刚登记完，把当前内容补推过去）。
//
// 必须清掉「上次推的 ETag」：登记往往发生在内容早就存在的时候，不清就等于认为
// 「内容没变、不用推」，手机要等到用户下一次复制才有东西可拿。
func (r *Relay) Kick() {
	r.mu.Lock()
	r.lastETag = ""
	r.mu.Unlock()
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

func (r *Relay) loop() {
	defer close(r.done)
	ticker := time.NewTicker(TickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-r.kick:
			r.tick()
		case <-ticker.C:
			r.tick()
		}
	}
}

// tick 检查缓存是否有新内容，有就对每台设备各推一次。
func (r *Relay) tick() {
	if !r.enabled() {
		return
	}
	text, etag, kind := r.engine.Current()
	if kind != clipboard.KindText || text == "" || etag == "" {
		return
	}
	r.mu.Lock()
	changed := etag != r.lastETag
	if changed {
		r.lastETag = etag
	}
	r.mu.Unlock()
	if !changed {
		return
	}

	_, hash := protocol.Digest(text)
	for _, t := range r.targets() {
		if t.DeviceID == "" || t.Token == "" || t.Fingerprint == "" || t.Host == "" {
			continue
		}
		r.mu.Lock()
		skip := r.pushed[t.DeviceID] == hash || r.inbound[hash] == t.DeviceID
		r.mu.Unlock()
		if skip {
			continue
		}
		go r.deliver(t, text, hash)
	}
}

// deliver 把一段内容推给一台设备，失败按 0s/2s/8s 重试三次。
func (r *Relay) deliver(t Target, text, hash string) {
	lock := r.deviceLock(t.DeviceID)
	lock.Lock()
	defer lock.Unlock()

	body, err := payload(text)
	if err != nil {
		r.report(t.DeviceID, err)
		return
	}
	var lastErr error
	for _, wait := range retryDelays {
		if wait > 0 {
			select {
			case <-r.stop:
				return
			case <-time.After(wait):
			}
		}
		// 内容已经被更新的复制覆盖：这条不必再送。同一台设备的推送是串行的，
		// 后一条一定排在前一条之后检查，所以不会出现旧内容后到、把新内容盖掉。
		if !r.stillCurrent(hash) {
			return
		}
		lastErr = r.send(t, body, hash)
		if lastErr == nil {
			r.mu.Lock()
			r.pushed[t.DeviceID] = hash
			delete(r.inbound, hash)
			r.mu.Unlock()
			r.report(t.DeviceID, nil)
			return
		}
	}
	r.logf("推送到设备 %s 失败: %v", t.DeviceID, lastErr)
	r.report(t.DeviceID, lastErr)
}

func (r *Relay) report(deviceID string, err error) {
	if r.onResult != nil {
		r.onResult(deviceID, err == nil, err)
	}
}

// deviceLock 返回该设备的串行锁，没有就建一个。
func (r *Relay) deviceLock(deviceID string) *sync.Mutex {
	r.clientMu.Lock()
	defer r.clientMu.Unlock()
	l, ok := r.deviceLocks[deviceID]
	if !ok {
		l = &sync.Mutex{}
		r.deviceLocks[deviceID] = l
	}
	return l
}

// stillCurrent 判断缓存里还是不是这条内容。
func (r *Relay) stillCurrent(hash string) bool {
	text, _, kind := r.engine.Current()
	if kind != clipboard.KindText || text == "" {
		return false
	}
	_, current := protocol.Digest(text)
	return current == hash
}

// payload 构造与 GET /api/clipboard 同构的请求体。
//
// 刻意不带 stamp：手机端的顺序号用于多设备互相推送时比较新旧，缺少时将电脑发来的内容
// 视为最新（老版本电脑也是这个行为）。带上电脑时钟反而可能因两端时钟差被当作旧内容丢弃，
// 那正是用户看到的「推了却没反应」。
func payload(text string) ([]byte, error) {
	p := protocol.BuildProfile(text, protocol.SourceDesktop)
	return json.Marshal(p)
}

// send 执行一次 PUT。返回 nil 表示手机确认收到。
func (r *Relay) send(t Target, body []byte, hash string) error {
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, t.url(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.Token)

	resp, err := r.clientFor(t).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// 响应体只有确认信息，读掉一小段让连接可以被回收，内容本身不记录。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return errors.New("手机已撤销这台电脑的推送授权")
	case protocol.StatusTooLarge:
		return fmt.Errorf("手机拒收：文本超过 %d KiB", protocol.MaxTextBytes/1024)
	default:
		return fmt.Errorf("手机返回 %d", resp.StatusCode)
	}
}

// clientFor 返回按「设备证书指纹 + 地址」缓存的客户端。
//
// 手机用的是自签证书、也没有合法主机名，所以关掉常规校验，改为在握手时比对叶证书指纹：
// 指纹来自手机自己登记，比证书链更有针对性。
func (r *Relay) clientFor(t Target) *http.Client {
	key := t.Fingerprint + "@" + t.Host
	r.clientMu.Lock()
	defer r.clientMu.Unlock()
	if c, ok := r.clients[key]; ok {
		return c
	}
	fp := strings.ToLower(t.Fingerprint)
	tr := &http.Transport{
		// 局域网直连：显式不代理，否则系统代理会把 192.168.x.x 也吞掉。
		Proxy:             nil,
		ForceAttemptHTTP2: false,
		DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, // 由 VerifyPeerCertificate 按指纹校验
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				if len(raw) == 0 {
					return errors.New("对端没有提供证书")
				}
				sum := sha256.Sum256(raw[0])
				if hex.EncodeToString(sum[:]) != fp {
					return errors.New("证书指纹与登记的不一致")
				}
				return nil
			},
		},
	}
	c := &http.Client{Transport: tr, Timeout: pushTimeout}
	// 设备上限是 8 台，超出时丢掉最早的一个，避免长时间运行后无限增长。
	if len(r.clients) >= 8 {
		for k, old := range r.clients {
			old.CloseIdleConnections()
			delete(r.clients, k)
			break
		}
	}
	r.clients[key] = c
	return c
}
