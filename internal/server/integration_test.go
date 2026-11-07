package server

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cyime-sync/internal/config"
	"cyime-sync/internal/pairing"
	"cyime-sync/internal/protocol"
	"cyime-sync/internal/relay"
)

// TestSamePortServesPlainAndTLS 验证单一端口同时服务两类客户端：
// 设置页送明文、手机端送 TLS，服务端靠首字节分流。
func TestSamePortServesPlainAndTLS(t *testing.T) {
	env := newEnv(t, "电脑上的内容")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: env.mgr.Handler()}
	go func() { _ = srv.Serve(&sniffListener{Listener: ln, conf: env.pm.TLSConfig()}) }()
	defer srv.Close()
	addr := ln.Addr().String()

	// 明文：本机浏览器打开设置页的路径。
	plain := &http.Client{Timeout: 5 * time.Second}
	if rec := getStatus(t, plain, "http://"+addr+"/api/status"); rec != http.StatusOK {
		t.Fatalf("明文状态接口应 200，实际 %d", rec)
	}

	// TLS：手机端路径。证书是自签的，测试里直接跳过链校验（真实客户端按指纹固定）。
	//nolint:gosec // 自签证书场景，手机端另有指纹校验
	secure := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}

	text, _, err := env.pm.QR([]pairing.Endpoint{{Host: "192.168.1.8", Port: 18740}})
	if err != nil {
		t.Fatal(err)
	}
	pid, secret := offerFromQR(t, text)
	body := fmt.Sprintf(`{"v":2,"pairing_id":%q,"secret":%q,"client_id":"6f1f3f9e-5f0b-4a1e-9c7d-0b8f2f4d3a11","client_name":"Pixel 8"}`, pid, secret)

	resp, err := secure.Post("https://"+addr+"/api/pair", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("TLS 配对请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("配对应 201，实际 %d：%s", resp.StatusCode, raw)
	}
	var pair struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pair); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodGet, "https://"+addr+"/api/clipboard", nil)
	req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	res, err := secure.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != protocol.StatusOK {
		t.Fatalf("带令牌读取应 200，实际 %d", res.StatusCode)
	}
	var profile protocol.Profile
	if err := json.NewDecoder(res.Body).Decode(&profile); err != nil {
		t.Fatal(err)
	}
	if profile.Text != "电脑上的内容" {
		t.Fatalf("读取内容不符: %q", profile.Text)
	}

	// 明文访问 v2 设备接口仍然需要有效令牌，避免明文被当成后门。
	req2, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/api/clipboard", nil)
	req2.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	res2, err := plain.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != protocol.StatusOK {
		t.Fatalf("明文携带有效令牌也应放行（协议不额外要求 TLS），实际 %d", res2.StatusCode)
	}
}

func getStatus(t *testing.T, c *http.Client, url string) int {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestPushReachesPhoneOnClipboardChange 串起整条主动推送链路：
// 引擎缓存变化 → 推送器按登记目标 PUT → 手机确认 → 设备被点亮为在线。
//
// 手机端扮演服务端（DeviceHost 监听 18741），电脑是发起方，这与真实方向一致。
func TestPushReachesPhoneOnClipboardChange(t *testing.T) {
	env := newEnv(t, "第一段")

	type hit struct {
		text string
		auth string
	}
	hits := make(chan hit, 4)
	phone := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(body, &in)
		hits <- hit{text: in.Text, auth: r.Header.Get("Authorization")}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"v":2}`))
	}))
	defer phone.Close()

	const pushToken = "abcdefghijklmnopqrstuvwxyz0123456789-_ABCDEFG"
	sum := sha256.Sum256(phone.Certificate().Raw)
	host, portText, err := net.SplitHostPort(strings.TrimPrefix(phone.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}

	deviceID := env.store.Get().Devices[0].ID
	saved, err := env.pm.SetPushTarget(deviceID, config.PushTarget{
		Host: host, Port: atoi(t, portText), Fingerprint: hex.EncodeToString(sum[:]),
		Token: pushToken, UpdatedAt: time.Now(),
	})
	if err != nil || !saved {
		t.Fatalf("登记推送目标失败: %v", err)
	}

	relaySvc := relay.New(env.engine,
		func() []relay.Target {
			var out []relay.Target
			for _, d := range env.store.Get().Devices {
				if d.Push == nil {
					continue
				}
				out = append(out, relay.Target{DeviceID: d.ID, Host: d.Push.Host, Port: d.Push.Port,
					Fingerprint: d.Push.Fingerprint, Token: d.Push.Token})
			}
			return out
		},
		func() bool { return env.store.Get().Enabled },
		func(id string, ok bool, _ error) {
			env.pm.SetPushFailed(id, !ok)
			if ok {
				env.pm.Touch(id)
			}
		}, nil)
	relaySvc.Start()
	defer relaySvc.Stop()

	// 模拟用户在电脑上复制了新内容。
	if _, err := env.clip.Write("第二段内容", env.clip.Sequence()); err != nil {
		t.Fatalf("写入剪贴板失败: %v", err)
	}
	env.engine.Refresh()

	select {
	case got := <-hits:
		if got.text != "第二段内容" {
			t.Fatalf("推送内容不对: %q", got.text)
		}
		if got.auth != "Bearer "+pushToken {
			t.Fatalf("推送应带手机发放的令牌，实际 %q", got.auth)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待手机收到推送超时")
	}

	// 推送成功也算一次可达：设备列表不能因此显示离线。
	if _, ok := env.pm.LastSeen(deviceID); !ok {
		t.Fatal("推送成功后应刷新设备在线状态")
	}
	if env.pm.PushFailed(deviceID) {
		t.Fatal("推送成功后不应保留失败标记")
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("端口格式不对: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}
