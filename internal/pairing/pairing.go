package pairing

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"cyime-sync/internal/config"
)

const (
	// OfferTTL 配对窗口有效期。手机端二维码提示"过期请刷新"与此一致。
	OfferTTL = 2 * time.Minute
	// MaxPairBody 配对请求体上限，与手机端一致。
	MaxPairBody = 4096
	// maxFailures 同一窗口内密钥错误次数上限，超过即关闭窗口。
	maxFailures = 10
	// tokenBytes 访问令牌随机字节数：base64url 后正好 43 字符（手机端按此校验）。
	tokenBytes = 32
)

// 配对错误：由 server 层映射为 HTTP 状态码。
var (
	ErrNoOffer      = errors.New("配对窗口未开启或已过期")
	ErrBadRequest   = errors.New("配对请求格式不正确")
	ErrBadSecret    = errors.New("配对密钥不匹配")
	ErrTooMany      = errors.New("已配对设备数量已达上限")
	ErrBodyTooLarge = errors.New("配对请求体过大")
)

// Endpoint 是二维码里告诉手机连哪个地址。
type Endpoint struct {
	Host string
	Port int
}

// 配对结果里只需要这两类对外事件，供设置页刷新。
const (
	EventPaired  = "paired"
	EventRevoked = "revoked"
)

// PairRequest 对应手机端 POST /api/pair 的请求体。
type PairRequest struct {
	V          int    `json:"v"`
	PairingID  string `json:"pairing_id"`
	Secret     string `json:"secret"`
	ClientID   string `json:"client_id"`
	ClientName string `json:"client_name"`
	ClientKind string `json:"client_kind"`
}

// PairResponse 是 201 响应体，手机端据此保存 server_id / device_id / access_token。
type PairResponse struct {
	V           int    `json:"v"`
	ServerID    string `json:"server_id"`
	ServerName  string `json:"server_name"`
	DeviceID    string `json:"device_id"`
	AccessToken string `json:"access_token"`
}

type offer struct {
	id        string
	secret    []byte // 32 字节原始密钥
	secretB64 string // 43 字符 base64url，二维码里传这个
	expires   time.Time
	failures  int
}

// Manager 持有证书、配对窗口与已配对设备。
type Manager struct {
	store *config.Store
	cert  tls.Certificate
	fpHex string
	name  string
	now   func() time.Time

	mu     sync.Mutex
	offer  *offer
	notify func(event, detail string)
}

// New 加载（或生成）证书并准备配对状态。
func New(store *config.Store, dir string) (*Manager, error) {
	cert, err := loadOrCreateCert(dir)
	if err != nil {
		return nil, err
	}
	return &Manager{
		store: store,
		cert:  cert,
		fpHex: fingerprint(cert),
		name:  sanitizeName(hostName(), 64, "CyIME 电脑"),
		now:   time.Now,
	}, nil
}

// SetNotifier 注册配对事件回调（设置页实时刷新用），可为 nil。
func (m *Manager) SetNotifier(fn func(event, detail string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notify = fn
}

// ServerID 是本机的稳定标识，二维码里作为 server_id。
func (m *Manager) ServerID() string { return m.store.Get().ServerID }

// ServerName 是对外展示的电脑名。
func (m *Manager) ServerName() string { return m.name }

// Fingerprint 是证书指纹（64 位十六进制）。
func (m *Manager) Fingerprint() string { return m.fpHex }

// TLSConfig 返回只用于本服务的 TLS 配置：仅 TLS1.2+，仅 HTTP/1.1。
func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{m.cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	}
}

// QR 返回当前配对二维码文本；没有有效窗口时新开一个。
//
// 复用未过期窗口而不是每次刷新都换密钥，否则用户截图/扫描过程中旧码会失效。
func (m *Manager) QR(eps []Endpoint) (string, time.Time, error) {
	if len(eps) == 0 {
		return "", time.Time{}, errors.New("没有可用的局域网地址")
	}
	m.mu.Lock()
	if m.offer == nil || !m.now().Before(m.offer.expires) {
		o, err := newOffer(m.now())
		if err != nil {
			m.mu.Unlock()
			return "", time.Time{}, err
		}
		m.offer = o
	}
	o := m.offer
	m.mu.Unlock()

	text, err := encodeQR(m.ServerID(), m.fpHex, eps, o.id, o.secret, o.expires)
	if err != nil {
		return "", time.Time{}, err
	}
	return text, o.expires, nil
}

// Rotate 作废当前窗口并立即开一个新的。
//
// 与 QR 的"复用未过期窗口"相反：设置页点「换一张」是用户主动放弃当前二维码，
// 旧 pairing_id/secret 随即失效（正在扫描的旧码会拿到 410）。
func (m *Manager) Rotate(eps []Endpoint) (string, time.Time, error) {
	if len(eps) == 0 {
		return "", time.Time{}, errors.New("没有可用的局域网地址")
	}
	m.mu.Lock()
	m.offer = nil
	m.mu.Unlock()
	return m.QR(eps)
}

// CloseOffer 立即关闭配对窗口。
func (m *Manager) CloseOffer() {
	m.mu.Lock()
	m.offer = nil
	m.mu.Unlock()
}

// Remaining 返回当前窗口剩余时间；窗口不存在时为 0。
func (m *Manager) Remaining() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.offer == nil {
		return 0
	}
	left := m.offer.expires.Sub(m.now())
	if left < 0 {
		return 0
	}
	return left
}

// Pair 处理一次扫码配对请求。
func (m *Manager) Pair(req PairRequest) (PairResponse, error) {
	m.mu.Lock()
	o := m.offer
	if o == nil || !m.now().Before(o.expires) {
		m.offer = nil
		m.mu.Unlock()
		return PairResponse{}, ErrNoOffer
	}
	if req.V != 2 || req.PairingID != o.id {
		m.mu.Unlock()
		return PairResponse{}, ErrBadRequest
	}
	if subtle.ConstantTimeCompare([]byte(req.Secret), []byte(o.secretB64)) != 1 {
		o.failures++
		if o.failures >= maxFailures {
			m.offer = nil
		}
		m.mu.Unlock()
		return PairResponse{}, ErrBadSecret
	}
	clientID := strings.ToLower(strings.TrimSpace(req.ClientID))
	if !config.IsUUID(clientID) {
		m.mu.Unlock()
		return PairResponse{}, ErrBadRequest
	}
	name := sanitizeName(req.ClientName, 64, "CyIME 设备")
	m.mu.Unlock()

	token, err := newAccessToken()
	if err != nil {
		return PairResponse{}, err
	}
	resp := PairResponse{
		V:           2,
		ServerID:    m.ServerID(),
		ServerName:  m.name,
		DeviceID:    "",
		AccessToken: token,
	}
	hash := hashToken(token)

	var cfg config.Config
	cfg, err = m.store.Update(func(c *config.Config) error {
		idx := -1
		for i, d := range c.Devices {
			if strings.EqualFold(d.ClientID, clientID) {
				idx = i
				break
			}
		}
		if idx < 0 && len(c.Devices) >= config.MaxDevices {
			return ErrTooMany
		}
		device := config.PairedDevice{
			ID:        config.NewUUID(),
			ClientID:  clientID,
			Name:      name,
			TokenHash: hash,
			CreatedAt: time.Now(),
		}
		if idx >= 0 {
			// 同一台手机重新配对：沿用原设备 id，只轮换令牌（旧令牌立即失效）。
			// 备注是用户起的名字，重新扫码不该丢。
			device.ID = c.Devices[idx].ID
			device.CreatedAt = c.Devices[idx].CreatedAt
			device.Note = c.Devices[idx].Note
			next := make([]config.PairedDevice, 0, len(c.Devices))
			next = append(next, device)
			next = append(next, c.Devices[:idx]...)
			next = append(next, c.Devices[idx+1:]...)
			c.Devices = next
			return nil
		}
		c.Devices = append([]config.PairedDevice{device}, c.Devices...)
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrTooMany) {
			return PairResponse{}, ErrTooMany
		}
		return PairResponse{}, err
	}
	resp.DeviceID = cfg.Devices[0].ID

	// 窗口一次性消费：成功配对后必须刷新二维码才能再配一台。
	m.mu.Lock()
	m.offer = nil
	notify := m.notify
	m.mu.Unlock()
	if notify != nil {
		notify(EventPaired, name)
	}
	return resp, nil
}

// Devices 返回已配对设备副本。
func (m *Manager) Devices() []config.PairedDevice {
	list := m.store.Get().Devices
	out := make([]config.PairedDevice, len(list))
	copy(out, list)
	return out
}

// Revoke 撤销指定设备，其令牌立即失效。
func (m *Manager) Revoke(id string) (bool, error) {
	var removed string
	var found bool
	if _, err := m.store.Update(func(c *config.Config) error {
		next := make([]config.PairedDevice, 0, len(c.Devices))
		for _, d := range c.Devices {
			if d.ID == id {
				found = true
				removed = d.Name
				continue
			}
			next = append(next, d)
		}
		c.Devices = next
		return nil
	}); err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	m.mu.Lock()
	notify := m.notify
	m.mu.Unlock()
	if notify != nil {
		notify(EventRevoked, removed)
	}
	return true, nil
}

// SetNote 更新设备备注（去空白并限长），返回是否找到该设备。
func (m *Manager) SetNote(id, note string) (bool, error) {
	note = config.NormalizeNote(note)
	found := false
	if _, err := m.store.Update(func(c *config.Config) error {
		for i := range c.Devices {
			if c.Devices[i].ID == id {
				c.Devices[i].Note = note
				found = true
				return nil
			}
		}
		return nil
	}); err != nil {
		return false, err
	}
	return found, nil
}

// Authenticate 校验访问令牌，返回对应的设备。
func (m *Manager) Authenticate(token string) (config.PairedDevice, bool) {
	if len(token) != 43 || !isBase64URL(token) {
		return config.PairedDevice{}, false
	}
	want := []byte(hashToken(token))
	for _, d := range m.store.Get().Devices {
		if subtle.ConstantTimeCompare([]byte(d.TokenHash), want) == 1 {
			return d, true
		}
	}
	return config.PairedDevice{}, false
}

func newOffer(now time.Time) (*offer, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("生成配对密钥失败: %w", err)
	}
	return &offer{
		id:        config.NewUUID(),
		secret:    secret,
		secretB64: base64.RawURLEncoding.EncodeToString(secret),
		expires:   now.Add(OfferTTL),
	}, nil
}

func newAccessToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成访问令牌失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func isBase64URL(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func hostName() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}

// sanitizeName 去掉控制字符并截断到 max 个字符，空值用 fallback。
func sanitizeName(s string, max int, fallback string) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	clean = strings.TrimSpace(clean)
	if r := []rune(clean); len(r) > max {
		clean = string(r[:max])
	}
	if clean == "" {
		return fallback
	}
	return clean
}
