package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cyime-sync/internal/clipboard"
	"cyime-sync/internal/protocol"
	"cyime-sync/internal/syncer"
)

const testToken = "abcdefghijklmnopqrstuvwxyz0123456789-_ABCDEFG"

type received struct {
	method string
	auth   string
	text   string
	hash   string
	size   int
	typ    string
	hasDat bool
	raw    string
}

// newTestEngine 造一个已经缓存了 initial 的引擎（内存剪贴板，无系统依赖）。
func newTestEngine(t *testing.T, initial string) *syncer.Engine {
	t.Helper()
	engine := syncer.New(clipboard.NewMemory(initial), syncer.NewRing(10))
	engine.Refresh()
	return engine
}

// startPhone 模拟手机端的 DeviceHost：只认固定令牌，记录收到的请求。
func startPhone(t *testing.T, token string, status int) (*httptest.Server, *[]received, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var got []received
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Hash    string `json:"hash"`
			Size    int    `json:"size"`
			HasData bool   `json:"has_data"`
		}
		_ = json.Unmarshal(body, &in)
		mu.Lock()
		got = append(got, received{
			method: r.Method, auth: r.Header.Get("Authorization"),
			text: in.Text, hash: in.Hash, size: in.Size, typ: in.Type, hasDat: in.HasData,
			raw: string(body),
		})
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"v":2}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &mu
}

// targetFor 把测试服务器转成推送目标，指纹取自它自己的证书。
func targetFor(t *testing.T, srv *httptest.Server, deviceID string) Target {
	t.Helper()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	if err != nil {
		t.Fatalf("解析测试服务器地址失败: %v", err)
	}
	sum := sha256.Sum256(srv.Certificate().Raw)
	var p int
	for _, c := range port {
		p = p*10 + int(c-'0')
	}
	return Target{DeviceID: deviceID, Host: host, Port: p,
		Fingerprint: hex.EncodeToString(sum[:]), Token: testToken}
}

func fastRetries(t *testing.T) {
	t.Helper()
	old := retryDelays
	retryDelays = []time.Duration{0}
	t.Cleanup(func() { retryDelays = old })
}

func TestRelayPushesCurrentClipboard(t *testing.T) {
	fastRetries(t)
	engine := newTestEngine(t, "复制的内容")
	srv, got, mu := startPhone(t, testToken, http.StatusOK)

	done := make(chan error, 1)
	r := New(engine, func() []Target { return []Target{targetFor(t, srv, "dev-1")} },
		func() bool { return true },
		func(_ string, ok bool, err error) { done <- err }, nil)

	r.tick()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("推送应成功，实际 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待推送结果超时")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("应只收到一次推送，实际 %d 次", len(*got))
	}
	ev := (*got)[0]
	if ev.method != http.MethodPut {
		t.Fatalf("应使用 PUT，实际 %s", ev.method)
	}
	if ev.text != "复制的内容" || ev.typ != "text" || ev.hasDat {
		t.Fatalf("请求体不符合协议: %+v", ev)
	}
	_, hash := protocol.Digest("复制的内容")
	if ev.hash != hash || ev.size != len("复制的内容") {
		t.Fatalf("hash/size 应由服务端重算: %+v", ev)
	}
	// 手机端 profile() 要求 data_name 显式为 null 或缺失，出现字符串就会被判非法。
	if strings.Contains(ev.raw, "data_name") {
		t.Fatalf("请求体不应携带 data_name: %s", ev.raw)
	}
	// 顺序号缺失表示「老版本电脑发来的，按最新处理」；带上电脑时钟反而可能被手机丢弃。
	if strings.Contains(ev.raw, "stamp") {
		t.Fatalf("请求体不应携带 stamp: %s", ev.raw)
	}
}

// 同一段内容第二次 tick 不应重复骚扰手机。
func TestRelayDoesNotRepeatSameContent(t *testing.T) {
	fastRetries(t)
	engine := newTestEngine(t, "同一段")
	srv, got, mu := startPhone(t, testToken, http.StatusOK)

	done := make(chan struct{}, 4)
	r := New(engine, func() []Target { return []Target{targetFor(t, srv, "dev-1")} },
		func() bool { return true },
		func(string, bool, error) { done <- struct{}{} }, nil)

	r.tick()
	<-done
	r.tick()
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("内容未变时不应重推，实际 %d 次", len(*got))
	}
}

// 手机自己推来的内容不再原样推回去：否则它刚复制的新内容可能被这条旧内容盖掉。
func TestRelaySkipsOriginDevice(t *testing.T) {
	fastRetries(t)
	engine := newTestEngine(t, "手机A推来的")
	srv, got, mu := startPhone(t, testToken, http.StatusOK)
	_, hash := protocol.Digest("手机A推来的")

	r := New(engine, func() []Target { return []Target{targetFor(t, srv, "dev-a")} },
		func() bool { return true }, func(string, bool, error) {}, nil)
	r.NoteInbound("dev-a", hash)

	r.tick()
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 0 {
		t.Fatalf("不应把内容推回给来源设备，实际 %d 次", len(*got))
	}
}

// 推送失败必须如实回报，状态接口据此让手机退回轮询。
func TestRelayReportsFailure(t *testing.T) {
	fastRetries(t)
	engine := newTestEngine(t, "推不出去")

	// 关掉的端口：连接一定失败。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	_ = ln.Close()

	results := make(chan error, 2)
	r := New(engine, func() []Target {
		return []Target{{DeviceID: "dead", Host: "127.0.0.1", Port: addr.Port,
			Fingerprint: strings.Repeat("ab", 32), Token: testToken}}
	}, func() bool { return true },
		func(_ string, ok bool, err error) {
			if ok {
				results <- nil
				return
			}
			results <- err
		}, nil)

	r.tick()
	select {
	case err := <-results:
		if err == nil {
			t.Fatal("端口关闭时应报告失败")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待失败回报超时")
	}
}

// 同步停用时不推送：此时手机端 GET 也只会拿到 503。
func TestRelayRespectsDisabled(t *testing.T) {
	fastRetries(t)
	engine := newTestEngine(t, "不该发")
	srv, got, mu := startPhone(t, testToken, http.StatusOK)

	r := New(engine, func() []Target { return []Target{targetFor(t, srv, "dev-1")} },
		func() bool { return false }, func(string, bool, error) {}, nil)
	r.tick()
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 0 {
		t.Fatalf("停用时不应推送，实际 %d 次", len(*got))
	}
}

// 手机在「内容早就存在」之后才登记：登记完必须补推一次，否则它要等到下次复制才有东西。
func TestKickPushesExistingContent(t *testing.T) {
	fastRetries(t)
	engine := newTestEngine(t, "早就复制好的")
	srv, got, mu := startPhone(t, testToken, http.StatusOK)

	var targets []Target
	done := make(chan struct{}, 4)
	r := New(engine, func() []Target { return targets },
		func() bool { return true },
		func(string, bool, error) { done <- struct{}{} }, nil)

	// 先 tick 一次把「当前内容」记下来，此时还没有任何设备。
	r.tick()
	time.Sleep(100 * time.Millisecond)
	targets = []Target{targetFor(t, srv, "dev-1")}

	// Kick 要真的走后台循环才有效（真实进程里 Start 一直在跑）。
	r.Start()
	defer r.Stop()
	r.Kick()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("登记后未补推已有内容")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 || (*got)[0].text != "早就复制好的" {
		t.Fatalf("应补推一次当前内容，实际 %+v", *got)
	}
}
