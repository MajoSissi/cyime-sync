package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cyime-sync/internal/clipboard"
	"cyime-sync/internal/config"
	"cyime-sync/internal/pairing"
	"cyime-sync/internal/protocol"
	"cyime-sync/internal/syncer"
)

type testEnv struct {
	mgr    *Manager
	clip   *clipboard.Memory
	store  *config.Store
	engine *syncer.Engine
	pm     *pairing.Manager
	// token 是一台"已配对设备"的访问令牌，供读写剪贴板的用例使用。
	token string
}

func newEnv(t *testing.T, initial string) *testEnv {
	t.Helper()
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatalf("配置初始化失败: %v", err)
	}
	clip := clipboard.NewMemory(initial)
	engine := syncer.New(clip, syncer.NewRing(50))
	engine.SetPushLimitFunc(func() int { return store.Get().MaxPushChars })
	engine.SetForceLimitFunc(func() int { return store.Get().ForcePushChars })
	engine.SetSkipGuessFunc(func() bool { return store.Get().SkipSuspectedOTP })
	engine.Refresh()
	pm, err := pairing.New(store, t.TempDir())
	if err != nil {
		t.Fatalf("配对模块初始化失败: %v", err)
	}
	mgr, err := New(Options{
		Config:         store,
		Engine:         engine,
		Version:        "test",
		Pairing:        pm,
		ApplyAutoStart: func(bool) error { return nil },
		Logf:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 避免设置变更时真的绑定端口。
	mgr.applyFn = func() error { return nil }
	return &testEnv{mgr: mgr, clip: clip, store: store, engine: engine, pm: pm, token: issueToken(t, pm)}
}

// issueToken 直接走配对接口授权一台虚拟设备，返回其访问令牌。
func issueToken(t *testing.T, pm *pairing.Manager) string {
	t.Helper()
	text, _, err := pm.QR([]pairing.Endpoint{{Host: "192.168.1.8", Port: 18740}})
	if err != nil {
		t.Fatalf("生成二维码失败: %v", err)
	}
	pid, secret := offerFromQR(t, text)
	resp, err := pm.Pair(pairing.PairRequest{
		V:          2,
		PairingID:  pid,
		Secret:     secret,
		ClientID:   "6f1f3f9e-5f0b-4a1e-9c7d-0b8f2f4d3a11",
		ClientName: "测试手机",
	})
	if err != nil {
		t.Fatalf("配对被拒: %v", err)
	}
	return resp.AccessToken
}

func (env *testEnv) do(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	env.mgr.Handler().ServeHTTP(rec, req)
	return rec
}

// local 构造"来自本机 + 带一次性令牌"的设置页请求。
func (env *testEnv) local(t *testing.T, method, path string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("X-Cyime-Token", env.mgr.token)
	return req
}

func clipboardReq(method, body, token string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/api/clipboard", nil)
	} else {
		r = httptest.NewRequest(method, "/api/clipboard", strings.NewReader(body))
	}
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestClipboardRequiresToken(t *testing.T) {
	env := newEnv(t, "内容")
	rec := env.do(t, clipboardReq(http.MethodGet, "", ""))
	if rec.Code != protocol.StatusUnauthorized {
		t.Fatalf("无令牌应 401，实际 %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("本版本不再提供 Basic 质询")
	}

	rec = env.do(t, clipboardReq(http.MethodGet, "", strings.Repeat("x", 43)))
	if rec.Code != protocol.StatusUnauthorized {
		t.Fatalf("未知令牌应 401，实际 %d", rec.Code)
	}
}

func TestClipboardGetReturnsProfile(t *testing.T) {
	env := newEnv(t, "你好 CyIME 🎉")
	rec := env.do(t, clipboardReq(http.MethodGet, "", env.token))
	if rec.Code != protocol.StatusOK {
		t.Fatalf("应 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control 应为 no-store，实际 %q", got)
	}
	var p protocol.Profile
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Text != "你好 CyIME 🎉" || p.HasData || p.Type != "text" {
		t.Fatalf("profile 不符: %+v", p)
	}
	if strings.Contains(rec.Body.String(), "data_name") {
		t.Fatal("响应不得包含 data_name")
	}
	size, hash := protocol.Digest(p.Text)
	if p.Size != size || p.Hash != hash {
		t.Fatalf("size/hash 应由服务端重算: %+v", p)
	}
	if rec.Header().Get("ETag") != protocol.ETag(p.Text) {
		t.Fatal("ETag 头不符")
	}
}

func TestClipboardGetNotModified(t *testing.T) {
	env := newEnv(t, "内容")
	etag := protocol.ETag("内容")
	req := clipboardReq(http.MethodGet, "", env.token)
	req.Header.Set("If-None-Match", etag)

	rec := env.do(t, req)
	if rec.Code != protocol.StatusNotModified {
		t.Fatalf("应 304，实际 %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("304 不得带响应体，实际 %q", rec.Body.String())
	}
}

func TestClipboardGetNoContent(t *testing.T) {
	env := newEnv(t, "内容")
	env.clip.SetNonText()
	env.engine.Refresh()

	rec := env.do(t, clipboardReq(http.MethodGet, "", env.token))
	if rec.Code != protocol.StatusNoContent {
		t.Fatalf("非文本应 204，实际 %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatal("204 不得带响应体")
	}
	if rec.Header().Get("ETag") != "" {
		t.Fatal("空剪贴板不产生 ETag")
	}
}

func TestClipboardPutRoundTrip(t *testing.T) {
	env := newEnv(t, "旧内容")
	text := "手机推来的多行\n第二行 😀"
	size, hash := protocol.Digest(text)
	body, _ := json.Marshal(protocol.Profile{
		Type: "text", Text: text, Size: size, Hash: hash,
		Source: protocol.SourceAndroid, HasData: false,
	})

	rec := env.do(t, clipboardReq(http.MethodPut, string(body), env.token))
	if rec.Code != protocol.StatusOK {
		t.Fatalf("应 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}

	var resp protocol.Profile
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Hash != hash {
		t.Fatalf("200 必须回显相同 hash（否则安卓端视为未确认）: %s != %s", resp.Hash, hash)
	}

	snap, err := env.clip.Read()
	if err != nil || snap.Text != text {
		t.Fatalf("剪贴板未写入: %v %q", err, snap.Text)
	}
}

func TestClipboardPutDuplicateIsSuccess(t *testing.T) {
	env := newEnv(t, "重复内容")
	size, hash := protocol.Digest("重复内容")
	body := `{"type":"text","text":"重复内容","size":` + itoa(size) + `,"hash":"` + hash + `"}`
	before := env.clip.Sequence()

	rec := env.do(t, clipboardReq(http.MethodPut, body, env.token))
	if rec.Code != protocol.StatusOK {
		t.Fatalf("重复推送应 200，实际 %d", rec.Code)
	}
	if env.clip.Sequence() != before {
		t.Fatal("重复推送不应重写剪贴板")
	}
}

func TestClipboardPutBadRequests(t *testing.T) {
	env := newEnv(t, "内容")
	cases := []struct {
		name string
		body string
		want int
	}{
		{"非 JSON", `oops`, protocol.StatusBadRequest},
		{"缺 text", `{"type":"text"}`, protocol.StatusBadRequest},
		{"size 不符", `{"text":"abc","size":1}`, protocol.StatusBadRequest},
		{"纯空白", `{"text":"   "}`, protocol.StatusBadRequest},
		{"超长文本", `{"text":"` + strings.Repeat("a", protocol.MaxTextBytes+1) + `"}`, protocol.StatusTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := env.do(t, clipboardReq(http.MethodPut, c.body, env.token))
			if rec.Code != c.want {
				t.Fatalf("期望 %d，实际 %d（%s）", c.want, rec.Code, rec.Body.String())
			}
		})
	}
}

// 超过推送上限的文本不进缓存：手机拿到 204，不会被大段内容卡住。
func TestOversizeTextNotPushed(t *testing.T) {
	limit := config.DefaultMaxPushChars
	env := newEnv(t, strings.Repeat("a", limit+1))
	if rec := env.do(t, clipboardReq(http.MethodGet, "", env.token)); rec.Code != http.StatusNoContent {
		t.Fatalf("超长文本应返回 204，实际 %d", rec.Code)
	}
	last, ok := env.engine.Ring().Last()
	if !ok || last.Status != syncer.StatusOversize {
		t.Fatalf("应记录一条超长跳过事件，实际 %+v", last)
	}
}

// 刚好等于上限仍然要推送，避免边界上"差一个字符就不同步"。
func TestLimitBoundaryStillPushed(t *testing.T) {
	env := newEnv(t, strings.Repeat("a", config.DefaultMaxPushChars))
	if rec := env.do(t, clipboardReq(http.MethodGet, "", env.token)); rec.Code != http.StatusOK {
		t.Fatalf("等于上限应正常推送，实际 %d", rec.Code)
	}
}

func TestDisabledServiceReturns503(t *testing.T) {
	env := newEnv(t, "内容")
	if _, err := env.store.Update(func(c *config.Config) error { c.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if rec := env.do(t, clipboardReq(http.MethodGet, "", env.token)); rec.Code != protocol.StatusUnavailable {
		t.Fatalf("停用后应 503，实际 %d", rec.Code)
	}
}

func TestSettingsRequiresLocalToken(t *testing.T) {
	env := newEnv(t, "内容")

	// 默认 RemoteAddr 非回环：直接拒绝。
	rec := env.do(t, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非本机应 403，实际 %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req.RemoteAddr = "127.0.0.1:51234"
	rec = env.do(t, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("本机但无令牌应 403，实际 %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req.RemoteAddr = "127.0.0.1:51234"
	req.Header.Set("X-Cyime-Token", env.mgr.token)
	rec = env.do(t, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("本机+令牌应 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	var view settingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Settings.Enabled {
		t.Fatal("设置应返回当前同步开关")
	}
	if view.Listen == "" {
		t.Fatal("设置应返回监听地址")
	}
}

// 设置页要同时读到两个上限：常规 1000、强制 10000。
func TestSettingsExposePushLimits(t *testing.T) {
	env := newEnv(t, "内容")
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req.RemoteAddr = "127.0.0.1:51234"
	req.Header.Set("X-Cyime-Token", env.mgr.token)

	rec := env.do(t, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", rec.Code)
	}
	var resp settingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Settings.MaxPushChars != config.DefaultMaxPushChars ||
		resp.Settings.ForcePushChars != config.DefaultForcePushChars {
		t.Fatalf("默认上限不符: %+v", resp.Settings)
	}
}

func TestSettingsUpdate(t *testing.T) {
	env := newEnv(t, "内容")
	req := httptest.NewRequest(http.MethodPut, "/api/settings",
		strings.NewReader(`{"port":19999,"max_push_chars":8000,"force_push_chars":20000,"skip_suspected_otp":false}`))
	req.RemoteAddr = "127.0.0.1:51234"
	req.Header.Set("X-Cyime-Token", env.mgr.token)
	req.Header.Set("Origin", "http://127.0.0.1:18740")

	rec := env.do(t, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	cfg := env.store.Get()
	// 默认是开启的，这里显式关掉，验证开关真的落到配置里。
	if cfg.Port != 19999 || cfg.MaxPushChars != 8000 || cfg.ForcePushChars != 20000 || cfg.SkipSuspectedOTP {
		t.Fatalf("设置未生效: %+v", cfg)
	}
	var resp settingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Settings.SkipSuspectedOTP || resp.Settings.ForcePushChars != 20000 {
		t.Fatalf("响应里的设置与配置不一致: %+v", resp.Settings)
	}

	// 强制上限同样要拦越界值。
	req = httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(`{"force_push_chars":0}`))
	req.RemoteAddr = "127.0.0.1:51234"
	req.Header.Set("X-Cyime-Token", env.mgr.token)
	req.Header.Set("Origin", "http://127.0.0.1:18740")
	if rec := env.do(t, req); rec.Code != http.StatusBadRequest {
		t.Fatalf("强制上限 0 应 400，实际 %d", rec.Code)
	}

	// 非法端口不落盘。
	req = httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(`{"port":70000}`))
	req.RemoteAddr = "127.0.0.1:51234"
	req.Header.Set("X-Cyime-Token", env.mgr.token)
	if rec := env.do(t, req); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法端口应 400，实际 %d", rec.Code)
	}
	if env.store.Get().Port != 19999 {
		t.Fatal("非法更新不应改变配置")
	}
}

func TestSettingsRejectsCrossOrigin(t *testing.T) {
	env := newEnv(t, "内容")
	req := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(`{"enabled":false}`))
	req.RemoteAddr = "127.0.0.1:51234"
	req.Header.Set("X-Cyime-Token", env.mgr.token)
	req.Header.Set("Origin", "http://evil.example.com")

	if rec := env.do(t, req); rec.Code != http.StatusForbidden {
		t.Fatalf("跨站写入应 403，实际 %d", rec.Code)
	}
}

// 配对窗口可由设置页主动开关：POST 打开、DELETE 关闭。
func TestPairingSessionOpenAndClose(t *testing.T) {
	env := newEnv(t, "内容")

	rec := env.do(t, env.local(t, http.MethodPost, "/api/pairing"))
	if rec.Code != http.StatusOK {
		t.Fatalf("打开窗口应 200，实际 %d", rec.Code)
	}
	var opened pairingView
	if err := json.Unmarshal(rec.Body.Bytes(), &opened); err != nil {
		t.Fatal(err)
	}
	if !opened.Open || opened.SecondsLeft <= 0 {
		t.Fatalf("窗口应处于打开状态: %+v", opened)
	}

	rec = env.do(t, env.local(t, http.MethodDelete, "/api/pairing"))
	if err := json.Unmarshal(rec.Body.Bytes(), &opened); err != nil {
		t.Fatal(err)
	}
	if opened.Open {
		t.Fatal("DELETE 后窗口应关闭")
	}
}

// POST 默认复用窗口，「换一张」（?rotate=1）才轮换密钥。
func TestPairingSessionRotate(t *testing.T) {
	env := newEnv(t, "内容")
	eps := []pairing.Endpoint{{Host: "192.168.1.8", Port: 18740}}

	first, _, err := env.pm.QR(eps)
	if err != nil {
		t.Fatal(err)
	}
	rec := env.do(t, env.local(t, http.MethodPost, "/api/pairing"))
	if rec.Code != http.StatusOK {
		t.Fatalf("刷新应 200，实际 %d", rec.Code)
	}
	again, _, err := env.pm.QR(eps)
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatal("普通 POST 不应轮换二维码")
	}

	rec = env.do(t, env.local(t, http.MethodPost, "/api/pairing?rotate=1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("换一张应 200，实际 %d", rec.Code)
	}
	rotated, _, err := env.pm.QR(eps)
	if err != nil {
		t.Fatal(err)
	}
	if rotated == first {
		t.Fatal("rotate=1 必须换一张新的二维码")
	}
}

func TestEventStreamSendsSnapshot(t *testing.T) {
	env := newEnv(t, "内容")
	srv := httptest.NewServer(env.mgr.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events?token="+env.mgr.token, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("连接事件流失败: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type 应为 text/event-stream，实际 %q", ct)
	}

	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读取首帧失败: %v", err)
	}
	if !strings.Contains(line, "event: snapshot") {
		t.Fatalf("首帧应为 snapshot，实际 %q", line)
	}
	data, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data, `"events"`) {
		t.Fatalf("snapshot 应包含事件列表，实际 %q", data)
	}
	_, _ = io.Copy(io.Discard, reader)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestQRCode(t *testing.T) {
	env := newEnv(t, "内容")
	rec := env.do(t, env.local(t, http.MethodGet, "/api/qr.png"))

	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type 应为 image/png，实际 %q", ct)
	}
	body := rec.Body.Bytes()
	if len(body) < 8 || !bytes.Equal(body[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		t.Fatal("返回内容不是合法 PNG")
	}
	if rec.Header().Get("X-Cyime-Pair-Expires") == "" {
		t.Fatal("应返回配对窗口过期时间")
	}
}

// 二维码内含一次性配对密钥，未带本机令牌时不能吐出。
func TestQRRequiresLocalToken(t *testing.T) {
	env := newEnv(t, "内容")
	rec := env.do(t, httptest.NewRequest(http.MethodGet, "/api/qr.png", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("无令牌应 403，实际 %d", rec.Code)
	}
}

// offerFromQR 从服务端生成的二维码文本里取出 pairing_id 与 secret，
// 模拟手机扫码后的行为。
func offerFromQR(t *testing.T, text string) (pairingID, secret string) {
	t.Helper()
	if !strings.HasPrefix(text, pairing.QRPrefix) {
		t.Fatalf("二维码前缀不符: %q", text)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(text, pairing.QRPrefix))
	if err != nil {
		t.Fatalf("二维码负载解码失败: %v", err)
	}
	n := int(raw[1])
	base := 2 + 6*n + 16 + 32
	if len(raw) < base+48 {
		t.Fatalf("二维码负载过短: %d", len(raw))
	}
	pid, err := config.ParseUUID(uuidText(raw[base : base+16]))
	if err != nil {
		t.Fatalf("pairing_id 非法: %v", err)
	}
	_ = pid
	return uuidText(raw[base : base+16]), base64.RawURLEncoding.EncodeToString(raw[base+16 : base+48])
}

func uuidText(b []byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func tlsRequest(method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Content-Type", "application/json")
	r.TLS = &tls.ConnectionState{} // 模拟经 HTTPS 到达
	return r
}

// TestPairOverTLSIssuesUsableToken 覆盖扫码配对的完整链路：
// 服务端二维码 → 手机 POST /api/pair → 拿到令牌 → 用令牌读写剪贴板与状态。
func TestPairOverTLSIssuesUsableToken(t *testing.T) {
	env := newEnv(t, "电脑上的内容")
	text, _, err := env.pm.QR([]pairing.Endpoint{{Host: "192.168.1.8", Port: 18740}})
	if err != nil {
		t.Fatal(err)
	}
	pid, secret := offerFromQR(t, text)

	body := fmt.Sprintf(`{"v":2,"pairing_id":%q,"secret":%q,"client_id":"6f1f3f9e-5f0b-4a1e-9c7d-0b8f2f4d3a11","client_name":"Pixel 8","client_kind":"android"}`, pid, secret)
	rec := env.do(t, tlsRequest(http.MethodPost, "/api/pair", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("配对应 201，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		V           int    `json:"v"`
		ServerID    string `json:"server_id"`
		DeviceID    string `json:"device_id"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if resp.V != 2 || resp.ServerID != env.pm.ServerID() || resp.DeviceID == "" {
		t.Fatalf("响应字段不符: %+v", resp)
	}
	if len(resp.AccessToken) != 43 {
		t.Fatalf("令牌长度应为 43，实际 %d", len(resp.AccessToken))
	}

	// 同一窗口不能复用。
	again := env.do(t, tlsRequest(http.MethodPost, "/api/pair", body))
	if again.Code != http.StatusGone {
		t.Fatalf("重复配对应 410，实际 %d", again.Code)
	}

	// 令牌可用：读取剪贴板。
	get := tlsRequest(http.MethodGet, "/api/clipboard", "")
	get.Header.Set("Authorization", "Bearer "+resp.AccessToken)
	rec = env.do(t, get)
	if rec.Code != protocol.StatusOK {
		t.Fatalf("带令牌应 200，实际 %d", rec.Code)
	}
	var profile protocol.Profile
	if err := json.Unmarshal(rec.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	if profile.Text != "电脑上的内容" || profile.HasData {
		t.Fatalf("返回内容不符: %+v", profile)
	}

	// 状态接口：手机端据此判断连接可用。
	status := tlsRequest(http.MethodGet, "/api/status", "")
	status.Header.Set("Authorization", "Bearer "+resp.AccessToken)
	rec = env.do(t, status)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"v":2`) {
		t.Fatalf("状态接口应返回 v2，实际 %d %s", rec.Code, rec.Body.String())
	}
}

func TestPairRequiresTLS(t *testing.T) {
	env := newEnv(t, "内容")
	req := httptest.NewRequest(http.MethodPost, "/api/pair", strings.NewReader(`{"v":2}`))
	rec := env.do(t, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("明文配对应 400，实际 %d", rec.Code)
	}
}

func TestPairWithoutWindowIsGone(t *testing.T) {
	env := newEnv(t, "内容")
	body := `{"v":2,"pairing_id":"6f1f3f9e-5f0b-4a1e-9c7d-0b8f2f4d3a11","secret":"` + strings.Repeat("A", 43) + `","client_id":"6f1f3f9e-5f0b-4a1e-9c7d-0b8f2f4d3a11"}`
	rec := env.do(t, tlsRequest(http.MethodPost, "/api/pair", body))
	if rec.Code != http.StatusGone {
		t.Fatalf("没有配对窗口应 410，实际 %d", rec.Code)
	}
}

// 方向开关取消后，v2 状态接口仍要报告双向可用，手机端据此显示"已连接"。
func TestV2StatusReportsBothDirections(t *testing.T) {
	env := newEnv(t, "内容")
	req := tlsRequest(http.MethodGet, "/api/status", "")
	req.Header.Set("Authorization", "Bearer "+env.token)
	rec := env.do(t, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态接口应 200，实际 %d", rec.Code)
	}
	var st struct {
		V         int  `json:"v"`
		Send      bool `json:"send_enabled"`
		Receive   bool `json:"receive_enabled"`
		Connected bool `json:"connected"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.V != 2 || !st.Connected || !st.Send || !st.Receive {
		t.Fatalf("状态响应不符: %+v", st)
	}
}

func TestV2RejectsUnknownToken(t *testing.T) {
	env := newEnv(t, "内容")
	req := tlsRequest(http.MethodGet, "/api/clipboard", "")
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 43))
	rec := env.do(t, req)
	if rec.Code != protocol.StatusUnauthorized {
		t.Fatalf("未知令牌应 401，实际 %d", rec.Code)
	}
}

// 撤销设备后令牌立即失效。
func TestRevokeDeviceInvalidatesToken(t *testing.T) {
	env := newEnv(t, "内容")
	text, _, err := env.pm.QR([]pairing.Endpoint{{Host: "192.168.1.8", Port: 18740}})
	if err != nil {
		t.Fatal(err)
	}
	pid, secret := offerFromQR(t, text)
	rec := env.do(t, tlsRequest(http.MethodPost, "/api/pair",
		fmt.Sprintf(`{"v":2,"pairing_id":%q,"secret":%q,"client_id":"6f1f3f9e-5f0b-4a1e-9c7d-0b8f2f4d3a11"}`, pid, secret)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("配对应 201，实际 %d", rec.Code)
	}
	var resp struct {
		DeviceID    string `json:"device_id"`
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	del := env.local(t, http.MethodDelete, "/api/devices?id="+resp.DeviceID)
	if rec := env.do(t, del); rec.Code != http.StatusOK {
		t.Fatalf("撤销应 200，实际 %d", rec.Code)
	}

	req := tlsRequest(http.MethodGet, "/api/clipboard", "")
	req.Header.Set("Authorization", "Bearer "+resp.AccessToken)
	if rec := env.do(t, req); rec.Code != protocol.StatusUnauthorized {
		t.Fatalf("撤销后应 401，实际 %d", rec.Code)
	}
}

// 设备备注：可写、可清空，且不影响该设备已有的令牌。
func TestDeviceNoteRoundTrip(t *testing.T) {
	env := newEnv(t, "内容")
	dev := env.pm.Devices()[0]

	patch := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/devices?id="+dev.ID, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:54321"
		req.Header.Set("X-Cyime-Token", env.mgr.token)
		return env.do(t, req)
	}

	if rec := patch(`{"note":"  工作机  "}`); rec.Code != http.StatusOK {
		t.Fatalf("设置备注应 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if got := env.pm.Devices()[0].Note; got != "工作机" {
		t.Fatalf("备注应去掉首尾空白，实际 %q", got)
	}
	if rec := env.do(t, clipboardReq(http.MethodGet, "", env.token)); rec.Code != http.StatusOK {
		t.Fatalf("改备注后令牌应仍然有效，实际 %d", rec.Code)
	}

	if rec := patch(`{"note":""}`); rec.Code != http.StatusOK {
		t.Fatalf("清空备注应 200，实际 %d", rec.Code)
	}
	if got := env.pm.Devices()[0].Note; got != "" {
		t.Fatalf("备注应已清空，实际 %q", got)
	}
}

// 设置页的配对状态必须包含设备列表，且不含令牌。
func TestPairingStateHidesSecrets(t *testing.T) {
	env := newEnv(t, "内容")
	if _, _, err := env.pm.QR([]pairing.Endpoint{{Host: "192.168.1.8", Port: 18740}}); err != nil {
		t.Fatal(err)
	}
	req := env.local(t, http.MethodGet, "/api/pairing")
	rec := env.do(t, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "token") || strings.Contains(body, "secret") || strings.Contains(body, "pairing_id") {
		t.Fatalf("配对状态不能包含密钥: %s", body)
	}
	if !strings.Contains(body, `"seconds_left"`) {
		t.Fatalf("应返回剩余时间: %s", body)
	}
}
