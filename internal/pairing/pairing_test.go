package pairing

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"cyime-sync/internal/config"
)

func newManager(t *testing.T) (*Manager, *config.Store) {
	t.Helper()
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatalf("配置初始化失败: %v", err)
	}
	m, err := New(store, t.TempDir())
	if err != nil {
		t.Fatalf("配对模块初始化失败: %v", err)
	}
	return m, store
}

// TestQRPayloadMatchesPhoneLayout 逐字节核对紧凑二维码布局。
// 手机端会校验版本、地址个数、固定长度 106+6n、UUID 与密钥长度，
// 任何字段顺序错误都会表现为"不是 CyIME 配对码"。
func TestQRPayloadMatchesPhoneLayout(t *testing.T) {
	m, _ := newManager(t)
	eps := []Endpoint{{Host: "192.168.1.8", Port: 18740}, {Host: "10.0.0.5", Port: 18740}}

	text, expires, err := m.QR(eps)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, QRPrefix) {
		t.Fatalf("二维码必须以 %q 开头，实际 %q", QRPrefix, text[:min(24, len(text))])
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(text, QRPrefix))
	if err != nil {
		t.Fatalf("base64url 解码失败: %v", err)
	}
	if want := 106 + 6*len(eps); len(raw) != want {
		t.Fatalf("负载长度应为 %d，实际 %d", want, len(raw))
	}
	if raw[0] != 2 {
		t.Fatalf("版本字节应为 2，实际 %d", raw[0])
	}
	if int(raw[1]) != len(eps) {
		t.Fatalf("地址个数应为 %d，实际 %d", len(eps), raw[1])
	}
	off := 2
	for _, ep := range eps {
		ip := net.IPv4(raw[off], raw[off+1], raw[off+2], raw[off+3]).String()
		port := int(raw[off+4])<<8 | int(raw[off+5])
		if ip != ep.Host || port != ep.Port {
			t.Fatalf("地址解析不符：得到 %s:%d，期望 %s:%d", ip, port, ep.Host, ep.Port)
		}
		off += 6
	}
	serverID := formatUUIDFrom(raw[off : off+16])
	if serverID != strings.ToLower(m.ServerID()) {
		t.Fatalf("server_id 不符：%s != %s", serverID, m.ServerID())
	}
	off += 16
	if got := hex.EncodeToString(raw[off : off+32]); got != m.Fingerprint() {
		t.Fatalf("证书指纹不符")
	}
	off += 32
	if _, err := config.ParseUUID(formatUUIDFrom(raw[off : off+16])); err != nil {
		t.Fatalf("pairing_id 不是合法 UUID: %v", err)
	}
	off += 16
	secret := raw[off : off+32]
	if base64.RawURLEncoding.EncodeToString(secret) != m.offer.secretB64 {
		t.Fatal("secret 与当前窗口不一致")
	}
	off += 32
	gotExpires := int64(binary.BigEndian.Uint64(raw[off : off+8]))
	if gotExpires != expires.Unix() {
		t.Fatalf("过期时间不符: %d != %d", gotExpires, expires.Unix())
	}
	if time.Until(expires) <= 0 || time.Until(expires) > OfferTTL {
		t.Fatalf("配对窗口有效期异常: %v", time.Until(expires))
	}
}

// formatUUIDFrom 把 16 字节还原为标准 UUID 文本。
func formatUUIDFrom(b []byte) string {
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}

func TestQRReusesWindowUntilExpiry(t *testing.T) {
	m, _ := newManager(t)
	eps := []Endpoint{{Host: "192.168.1.8", Port: 18740}}
	first, _, err := m.QR(eps)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := m.QR(eps)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("窗口未过期时不应更换二维码，否则用户扫描中途会失效")
	}

	base := time.Now()
	m.now = func() time.Time { return base.Add(OfferTTL + time.Second) }
	third, _, err := m.QR(eps)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("窗口过期后应生成新的二维码")
	}
}

func TestRotateReplacesWindow(t *testing.T) {
	m, _ := newManager(t)
	eps := []Endpoint{{Host: "192.168.1.8", Port: 18740}}

	first, _, err := m.QR(eps)
	if err != nil {
		t.Fatal(err)
	}
	old := *m.offer

	second, _, err := m.Rotate(eps)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("「换一张」必须给出不同的二维码")
	}
	if m.offer.id == old.id || m.offer.secretB64 == old.secretB64 {
		t.Fatal("轮换后 pairing_id 与 secret 都必须更换")
	}

	// 旧码失效：pairing_id 已不在窗口里，按字段不符拒绝，不能凭旧凭据配上。
	_, err = m.Pair(PairRequest{
		V: 2, PairingID: old.id, Secret: old.secretB64,
		ClientID: "6f1f3f9e-5f0b-4a1e-9c7d-0b8f2f4d3a11",
	})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("旧码应被拒绝，实际 %v", err)
	}

	// 没有可用地址时不能顺手把窗口清掉，否则一次失败的请求就废掉用户的二维码。
	if _, _, err := m.Rotate(nil); err == nil {
		t.Fatal("没有地址时应返回错误")
	}
	if m.offer == nil {
		t.Fatal("轮换失败不应关闭现有窗口")
	}
}

// 在线状态依赖"最近一次认证时间"：配对成功即点亮，认证会刷新，撤销后清掉。
func TestLastSeenFollowsAuthentication(t *testing.T) {
	m, _ := newManager(t)
	if _, _, err := m.QR([]Endpoint{{Host: "192.168.1.8", Port: 18740}}); err != nil {
		t.Fatal(err)
	}
	resp, err := m.Pair(PairRequest{
		V: 2, PairingID: m.offer.id, Secret: m.offer.secretB64,
		ClientID: "6f1f3f9e-5f0b-4a1e-9c7d-0b8f2f4d3a11", ClientName: "Pixel 8",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.LastSeen(resp.DeviceID); !ok {
		t.Fatal("配对成功后应立即算作活动过")
	}

	m.clearSeen(resp.DeviceID)
	if _, ok := m.LastSeen(resp.DeviceID); ok {
		t.Fatal("清空后不应还有记录")
	}
	if _, ok := m.Authenticate(resp.AccessToken); !ok {
		t.Fatal("配对返回的令牌应当有效")
	}
	if _, ok := m.LastSeen(resp.DeviceID); !ok {
		t.Fatal("认证通过后应刷新活动时间")
	}

	if _, err := m.Revoke(resp.DeviceID); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.LastSeen(resp.DeviceID); ok {
		t.Fatal("撤销后应清掉活动记录")
	}
}

func TestPairIssuesTokenAndRotatesOnRepair(t *testing.T) {
	m, store := newManager(t)
	if _, _, err := m.QR([]Endpoint{{Host: "192.168.1.8", Port: 18740}}); err != nil {
		t.Fatal(err)
	}
	req := PairRequest{
		V: 2, PairingID: m.offer.id, Secret: m.offer.secretB64,
		ClientID: "6f1f3f9e-5f0b-4a1e-9c7d-0b8f2f4d3a11", ClientName: "Pixel 8",
	}
	resp, err := m.Pair(req)
	if err != nil {
		t.Fatalf("配对失败: %v", err)
	}
	if resp.V != 2 || resp.ServerID != m.ServerID() || resp.ServerName == "" {
		t.Fatalf("响应字段不完整: %+v", resp)
	}
	if len(resp.AccessToken) != 43 {
		t.Fatalf("访问令牌长度应为 43，实际 %d", len(resp.AccessToken))
	}
	dev, ok := m.Authenticate(resp.AccessToken)
	if !ok {
		t.Fatal("新令牌应通过校验")
	}
	if dev.Name != "Pixel 8" {
		t.Fatalf("设备名不符: %q", dev.Name)
	}
	if got := store.Get().Devices; len(got) != 1 || got[0].TokenHash == resp.AccessToken {
		t.Fatal("配置里只能保存令牌哈希")
	}

	// 同一台手机重新配对：沿用设备 id，旧令牌立即失效。
	if _, err := store.Update(func(c *config.Config) error {
		c.Devices[0].Note = "工作机"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	oldID := dev.ID
	if _, _, err := m.QR([]Endpoint{{Host: "192.168.1.8", Port: 18740}}); err != nil {
		t.Fatal(err)
	}
	req.PairingID, req.Secret = m.offer.id, m.offer.secretB64
	again, err := m.Pair(req)
	if err != nil {
		t.Fatal(err)
	}
	if again.DeviceID != oldID {
		t.Fatalf("重新配对应沿用设备 id：%s != %s", again.DeviceID, oldID)
	}
	// 备注是用户起的名字，重新扫码不该丢。
	if got := store.Get().Devices[0].Note; got != "工作机" {
		t.Fatalf("重新配对丢了备注: %q", got)
	}
	if _, ok := m.Authenticate(resp.AccessToken); ok {
		t.Fatal("旧令牌必须失效")
	}
	if _, ok := m.Authenticate(again.AccessToken); !ok {
		t.Fatal("新令牌应可用")
	}
}

func TestPairRejectsWrongSecretAndExpiredWindow(t *testing.T) {
	m, _ := newManager(t)
	eps := []Endpoint{{Host: "192.168.1.8", Port: 18740}}
	if _, _, err := m.QR(eps); err != nil {
		t.Fatal(err)
	}
	good := m.offer

	bad := PairRequest{V: 2, PairingID: good.id, Secret: strings.Repeat("A", 43),
		ClientID: "6f1f3f9e-5f0b-4a1e-9c7d-0b8f2f4d3a11"}
	if _, err := m.Pair(bad); err != ErrBadSecret {
		t.Fatalf("错误密钥应返回 ErrBadSecret，实际 %v", err)
	}
	if _, err := m.Pair(PairRequest{V: 2, PairingID: good.id, Secret: good.secretB64, ClientID: "not-a-uuid"}); err != ErrBadRequest {
		t.Fatalf("非法 client_id 应返回 ErrBadRequest，实际 %v", err)
	}

	m.now = func() time.Time { return time.Now().Add(OfferTTL + time.Minute) }
	if _, err := m.Pair(PairRequest{V: 2, PairingID: good.id, Secret: good.secretB64,
		ClientID: "6f1f3f9e-5f0b-4a1e-9c7d-0b8f2f4d3a11"}); err != ErrNoOffer {
		t.Fatalf("过期窗口应返回 ErrNoOffer，实际 %v", err)
	}
}

func TestPairClosesAfterTooManyFailures(t *testing.T) {
	m, _ := newManager(t)
	if _, _, err := m.QR([]Endpoint{{Host: "192.168.1.8", Port: 18740}}); err != nil {
		t.Fatal(err)
	}
	bad := PairRequest{V: 2, PairingID: m.offer.id, Secret: strings.Repeat("B", 43),
		ClientID: "6f1f3f9e-5f0b-4a1e-9c7d-0b8f2f4d3a11"}
	for i := 0; i < maxFailures; i++ {
		_, _ = m.Pair(bad)
	}
	if m.Remaining() != 0 {
		t.Fatal("连续猜错后应关闭配对窗口")
	}
}

func TestRevokeInvalidatesToken(t *testing.T) {
	m, _ := newManager(t)
	if _, _, err := m.QR([]Endpoint{{Host: "192.168.1.8", Port: 18740}}); err != nil {
		t.Fatal(err)
	}
	resp, err := m.Pair(PairRequest{V: 2, PairingID: m.offer.id, Secret: m.offer.secretB64,
		ClientID: "6f1f3f9e-5f0b-4a1e-9c7d-0b8f2f4d3a11", ClientName: "平板"})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := m.Revoke(resp.DeviceID)
	if err != nil || !ok {
		t.Fatalf("撤销失败: ok=%v err=%v", ok, err)
	}
	if _, ok := m.Authenticate(resp.AccessToken); ok {
		t.Fatal("撤销后令牌必须失效")
	}
	if ok, _ := m.Revoke("00000000-0000-4000-8000-000000000000"); ok {
		t.Fatal("不存在的设备不应报告成功")
	}
}

func TestEncodeQRRejectsBadInput(t *testing.T) {
	m, _ := newManager(t)
	cases := []struct {
		name string
		eps  []Endpoint
	}{
		{"没有地址", nil},
		{"域名而非 IP", []Endpoint{{Host: "example.com", Port: 18740}}},
		{"端口越界", []Endpoint{{Host: "192.168.1.8", Port: 70000}}},
	}
	for _, c := range cases {
		if _, _, err := m.QR(c.eps); err == nil {
			t.Fatalf("%s 时应报错", c.name)
		}
	}
}

func TestCertificateIsStableAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := New(store, dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(store, dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint() != second.Fingerprint() {
		t.Fatal("证书指纹必须跨重启保持，否则手机端会认为换了电脑")
	}
	if len(first.Fingerprint()) != 64 {
		t.Fatalf("指纹应为 64 位十六进制，实际 %q", first.Fingerprint())
	}
	if !bytes.Equal(first.cert.Certificate[0], second.cert.Certificate[0]) {
		t.Fatal("应复用磁盘上的证书")
	}
}
