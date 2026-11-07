// Package config 负责配置结构与读写：单个 JSON 文件、原子落盘、宽松迁移。
//
// 配置文件位于可执行文件同级的 data/config.json（见 internal/appdir）。文件可手工
// 编辑，读取时做校验与字段修复，绝不因为单个字段非法而丢弃整份配置。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// SchemaVersion 当前配置结构版本，用于后续迁移。
	// 3: 移除 v1 配对码，仅保留 v2 扫码配对。
	// 4: 方向开关与预览开关取消（恒开），新增推送字符上限与设备备注。
	// 5: 新增「跳过疑似验证码」开关。
	// 6: 默认推送上限降到 1000，新增强制推送上限。
	SchemaVersion = 6

	// DefaultPort 默认监听端口。
	DefaultPort = 18740
	// DefaultListen 默认监听地址：0.0.0.0 以便手机端局域网可达。
	DefaultListen = "0.0.0.0"
	// FileName 配置文件名（位于数据目录下）。
	FileName = "config.json"

	// MaxDevices 允许同时配对的最多设备数（与手机端保存上限一致）。
	MaxDevices = 8

	// DefaultMaxPushChars 默认推送字符上限：超过这个长度的文本不会被推到手机。
	DefaultMaxPushChars = 1000
	// DefaultForcePushChars 强制推送（托盘菜单「推送」）的字符上限，比普通上限宽松。
	DefaultForcePushChars = 10000
	// MinPushChars / MaxPushChars 两个上限的允许取值区间。
	MinPushChars = 1
	MaxPushChars = 100000

	// legacyMaxPushChars 是 v6 之前的默认值。升级时只在用户没改过（仍是该值）
	// 的情况下跟随新默认值，避免覆盖自定义设置。
	legacyMaxPushChars = 5000

	// MaxNoteRunes 设备备注最大长度。
	MaxNoteRunes = 32
)

// Config 是持久化的全部设置。字段顺序即落盘顺序，便于人工阅读。
type Config struct {
	Version       int    `json:"version"`
	Enabled       bool   `json:"enabled"`
	ListenAddress string `json:"listen_address"`
	Port          int    `json:"port"`
	// ServerID 本机在 v2 配对里的稳定标识（二维码中的 server_id），首次启动生成。
	ServerID string `json:"server_id"`
	// Devices 已通过扫码配对的手机/平板，只保存令牌哈希。
	Devices []PairedDevice `json:"devices,omitempty"`
	// MaxPushChars 单次推送的字符上限（ASCII 与中文都按 1 个字符计）。
	// 超过上限的内容不会推到手机，避免大段文本把两端都卡住。
	MaxPushChars int `json:"max_push_chars"`
	// ForcePushChars 手动「推送」（托盘菜单）时使用的字符上限，比常规上限宽松：
	// 用户主动要求推送，可以接受更长的内容，但也不能真的把两端卡死。
	ForcePushChars int `json:"force_push_chars"`
	// SkipSuspectedOTP 跳过"疑似短信验证码"（4-8 位纯数字短文本），默认开启。
	// 只影响本程序的形态猜测；内容来源自己声明的隐私标记始终跳过，不受此开关影响。
	SkipSuspectedOTP bool `json:"skip_suspected_otp"`
	// AutoStart 开机自启动（HKCU Run），默认关闭。
	AutoStart bool `json:"auto_start"`
}

// PairedDevice 是一个已授权读取/写入本机剪贴板的设备。
//
// 令牌本身不落盘：只保存 SHA-256，避免配置文件泄露后直接冒用。
type PairedDevice struct {
	ID       string `json:"id"`
	ClientID string `json:"client_id"`
	// Name 是手机上报的型号名（如 V2352A），由对端决定。
	Name string `json:"name"`
	// Note 是用户自定义备注，用于区分同型号设备；为空时界面显示 Name。
	Note      string    `json:"note,omitempty"`
	TokenHash string    `json:"token_hash"`
	CreatedAt time.Time `json:"created_at"`
	// Push 非空表示这台设备允许电脑主动推送（手机端 POST /api/peer 登记的结果）。
	Push *PushTarget `json:"push,omitempty"`
}

// PushTarget 是手机反向注册的推送地址。
//
// 新版 CyIME 手机端（2026-11 起）默认不再每 2 秒轮询电脑，而是把自己的监听端口、
// 证书指纹和一次性授权令牌交给电脑，由电脑在剪贴板变化时直接 PUT 过去。
// 没有这条记录时手机退回旧版轮询，而旧轮询只在输入法键盘显示时生效，
// 于是「复制了手机却没收到」在键盘收起时必然发生。
type PushTarget struct {
	// HostID 是手机自己报告的设备标识（仅用于排障，不参与寻址）。
	HostID string `json:"host_id,omitempty"`
	// Host 是手机在局域网里的地址：取它发起登记请求时的来源地址。
	Host string `json:"host"`
	// Port 是手机 DeviceHost 的监听端口（默认 18741）。
	Port int `json:"port"`
	// Fingerprint 是手机证书叶 DER 的 SHA-256 小写十六进制，用于固定信任。
	Fingerprint string `json:"fingerprint"`
	// Token 是手机发给电脑的 Bearer 令牌（43 字符 base64url）。
	Token     string    `json:"token"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Valid 判断推送目标是否可用（地址/端口/指纹/令牌都在合理范围内）。
func (p PushTarget) Valid() bool {
	if p.Host == "" || p.Port < 1 || p.Port > 65535 {
		return false
	}
	if len(p.Fingerprint) != 64 || len(p.Token) != 43 {
		return false
	}
	for _, r := range p.Fingerprint {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	for _, r := range p.Token {
		if (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// Default 返回可直接使用的默认配置。
func Default() Config {
	return Config{
		Version:          SchemaVersion,
		Enabled:          true,
		ListenAddress:    DefaultListen,
		Port:             DefaultPort,
		ServerID:         NewUUID(),
		MaxPushChars:     DefaultMaxPushChars,
		ForcePushChars:   DefaultForcePushChars,
		SkipSuspectedOTP: true,
		AutoStart:        false,
	}
}

// Listen 返回 host:port 形式的监听地址。
func (c Config) Listen() string {
	host := c.ListenAddress
	if host == "" {
		host = DefaultListen
	}
	return fmt.Sprintf("%s:%d", host, c.Port)
}

// Validate 校验关键字段，返回人类可读的错误。
func (c Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("端口必须在 1-65535 之间，当前为 %d", c.Port)
	}
	if c.MaxPushChars < MinPushChars || c.MaxPushChars > MaxPushChars {
		return fmt.Errorf("推送字符上限需在 %d-%d 之间，当前为 %d", MinPushChars, MaxPushChars, c.MaxPushChars)
	}
	if c.ForcePushChars < MinPushChars || c.ForcePushChars > MaxPushChars {
		return fmt.Errorf("手动推送字符上限需在 %d-%d 之间，当前为 %d", MinPushChars, MaxPushChars, c.ForcePushChars)
	}
	return ValidateListenAddress(c.ListenAddress)
}

// NormalizeNote 规整用户填写的设备备注：去首尾空白、折叠内部空白、限长。
func NormalizeNote(note string) string {
	note = strings.Join(strings.Fields(note), " ")
	r := []rune(note)
	if len(r) > MaxNoteRunes {
		note = string(r[:MaxNoteRunes])
	}
	return note
}

// ValidateListenAddress 只允许空、0.0.0.0、localhost 或合法 IP 字面量。
func ValidateListenAddress(addr string) error {
	switch addr {
	case "", "0.0.0.0", "127.0.0.1", "localhost", "::1", "::":
		return nil
	}
	if strings.ContainsAny(addr, " \t\r\n/") {
		return fmt.Errorf("监听地址不能包含空白或路径分隔符：%q", addr)
	}
	if strings.Contains(addr, ":") && !strings.Contains(addr, "::") {
		// 带端口的写法一律拒绝，端口由 Port 字段单独管理。
		return fmt.Errorf("监听地址只填 IP，不要带端口：%q", addr)
	}
	return nil
}

// Store 提供并发安全的配置访问与原子落盘。
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

// Open 读取（或初始化）数据目录下的 config.json。
// 返回的 error 表示发生了可恢复的问题（如文件损坏已备份、字段被修复），
// Store 始终可用。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建配置目录失败: %w", err)
	}
	path := filepath.Join(dir, FileName)
	s := &Store{path: path}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg := Default()
		s.cfg = cfg
		if err := s.save(cfg); err != nil {
			return s, err
		}
		return s, nil
	}
	if err != nil {
		s.cfg = Default()
		return s, fmt.Errorf("读取配置失败，已使用默认值: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		// 损坏文件备份后回退默认值，避免用户彻底失去启动能力。
		_ = os.Rename(path, path+".bak")
		s.cfg = Default()
		if saveErr := s.save(s.cfg); saveErr != nil {
			return s, fmt.Errorf("配置损坏且无法重建: %v (原始错误: %w)", saveErr, err)
		}
		return s, fmt.Errorf("配置损坏，已备份为 config.json.bak 并重建: %w", err)
	}

	issues := normalize(&cfg)
	s.cfg = cfg
	if len(issues) > 0 {
		if err := s.save(cfg); err != nil {
			return s, err
		}
		return s, fmt.Errorf("配置已修复: %s", strings.Join(issues, "; "))
	}
	return s, nil
}

// normalize 就地修复缺失/非法字段，返回修复说明。
func normalize(c *Config) []string {
	var issues []string
	// v5 才引入「跳过疑似验证码」，旧配置里没有该字段（读到的是零值 false）。
	// 迁移时置为 true，保持升级前后的同步行为完全一致，不让用户"莫名其妙开始同步验证码"。
	if c.Version > 0 && c.Version < 5 {
		c.SkipSuspectedOTP = true
	}
	// v6 把默认推送上限从 5000 降到 1000。只在用户从未改过时跟随，否则会覆盖自定义值。
	if c.Version > 0 && c.Version < 6 {
		if c.MaxPushChars == legacyMaxPushChars {
			c.MaxPushChars = DefaultMaxPushChars
		}
		c.ForcePushChars = DefaultForcePushChars
	}
	if c.Version <= 0 {
		c.Version = SchemaVersion
	}
	if c.Port == 0 {
		c.Port = DefaultPort
		issues = append(issues, "端口缺失，已重置为默认值")
	}
	if c.Port < 1 || c.Port > 65535 {
		c.Port = DefaultPort
		issues = append(issues, "端口非法，已重置为默认值")
	}
	if c.ListenAddress == "" {
		c.ListenAddress = DefaultListen
	}
	if ValidateListenAddress(c.ListenAddress) != nil {
		c.ListenAddress = DefaultListen
		issues = append(issues, "监听地址非法，已重置为 0.0.0.0")
	}
	if !IsUUID(c.ServerID) {
		c.ServerID = NewUUID()
		issues = append(issues, "缺少本机标识，已重新生成")
	}
	if c.MaxPushChars == 0 {
		c.MaxPushChars = DefaultMaxPushChars
		issues = append(issues, "推送字符上限缺失，已重置为默认值")
	} else if c.MaxPushChars < MinPushChars || c.MaxPushChars > MaxPushChars {
		c.MaxPushChars = min(max(c.MaxPushChars, MinPushChars), MaxPushChars)
		issues = append(issues, "推送字符上限超出范围，已修正")
	}
	if c.ForcePushChars == 0 {
		c.ForcePushChars = DefaultForcePushChars
		issues = append(issues, "手动推送字符上限缺失，已重置为默认值")
	} else if c.ForcePushChars < MinPushChars || c.ForcePushChars > MaxPushChars {
		c.ForcePushChars = min(max(c.ForcePushChars, MinPushChars), MaxPushChars)
		issues = append(issues, "手动推送字符上限超出范围，已修正")
	}
	if len(c.Devices) > MaxDevices {
		c.Devices = c.Devices[:MaxDevices]
		issues = append(issues, "已配对设备数量超限，已截断")
	}
	kept := c.Devices[:0]
	for _, d := range c.Devices {
		if IsUUID(d.ID) && d.TokenHash != "" {
			d.Note = NormalizeNote(d.Note)
			// 推送目标来自网络输入：手改坏了就丢掉，别让推送循环一直拨一个不存在的地址。
			if d.Push != nil && !d.Push.Valid() {
				d.Push = nil
				issues = append(issues, "已丢弃无效的推送地址")
			}
			kept = append(kept, d)
		}
	}
	if len(kept) != len(c.Devices) {
		c.Devices = kept
		issues = append(issues, "已移除无效的配对设备记录")
	}
	if c.Version < SchemaVersion {
		c.Version = SchemaVersion
		issues = append(issues, "配置结构已升级")
	}
	return issues
}

// Get 返回配置副本。
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Path 返回配置文件绝对路径。
func (s *Store) Path() string { return s.path }

// Update 在锁内修改配置：fn 返回错误则不落盘、不生效。
// 成功时返回落盘后的新配置。
func (s *Store) Update(fn func(*Config) error) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cfg
	if err := fn(&next); err != nil {
		return s.cfg, err
	}
	if err := next.Validate(); err != nil {
		return s.cfg, err
	}
	if err := s.save(next); err != nil {
		return s.cfg, fmt.Errorf("保存配置失败: %w", err)
	}
	s.cfg = next
	return next, nil
}

// save 原子落盘：同目录临时文件 + rename，权限 0600。
// 调用方必须持有 s.mu（或处于初始化阶段）。
func (s *Store) save(cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后此调用无效

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	return nil
}
