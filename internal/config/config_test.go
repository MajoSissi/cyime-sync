package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenCreatesDefaultConfig(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("首次读取不应报错: %v", err)
	}
	cfg := store.Get()
	if !cfg.Enabled || cfg.Port != DefaultPort || cfg.ListenAddress != DefaultListen {
		t.Fatalf("默认值不符: %+v", cfg)
	}
	if !IsUUID(cfg.ServerID) {
		t.Fatalf("默认本机标识非法: %q", cfg.ServerID)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatalf("配置文件未落盘: %v", err)
	}
}

func TestDeviceLimitAndSanitizing(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	for i := 0; i < MaxDevices+3; i++ {
		cfg.Devices = append(cfg.Devices, PairedDevice{
			ID: NewUUID(), ClientID: NewUUID(), Name: "设备", TokenHash: "h",
		})
	}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, FileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err == nil {
		t.Fatal("超限设备应产生修复提示")
	}
	if got := len(store.Get().Devices); got != MaxDevices {
		t.Fatalf("设备数应截断到 %d，实际 %d", MaxDevices, got)
	}
}

func TestUpdateValidatesAndPersists(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.Update(func(c *Config) error { c.Port = 0; return nil }); err == nil {
		t.Fatal("端口 0 应被拒绝")
	}
	if store.Get().Port != DefaultPort {
		t.Fatal("失败更新不应改变内存配置")
	}

	if _, err := store.Update(func(c *Config) error { c.Port = 19999; return nil }); err != nil {
		t.Fatalf("合法更新失败: %v", err)
	}

	reopened, err := Open(filepath.Dir(store.Path()))
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	if reopened.Get().Port != 19999 {
		t.Fatal("配置未落盘")
	}
}

func TestCorruptConfigRecovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err == nil {
		t.Fatal("损坏的配置应返回可恢复错误")
	}
	if !IsUUID(store.Get().ServerID) {
		t.Fatalf("应重建出可用的默认配置: %+v", store.Get())
	}
	if _, statErr := os.Stat(path + ".bak"); statErr != nil {
		t.Fatal("损坏文件应备份为 config.json.bak")
	}
}

func TestNormalizeFixesIllegalFields(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.Port = 70000
	cfg.ListenAddress = "192.168.1.5:9999"
	cfg.ServerID = "not-a-uuid"
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, FileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := Open(dir)
	if err == nil {
		t.Fatal("非法字段应产生修复提示")
	}
	got := store.Get()
	if got.Port != DefaultPort || got.ListenAddress != DefaultListen {
		t.Fatalf("字段未被修复: %+v", got)
	}
	if !IsUUID(got.ServerID) {
		t.Fatalf("本机标识未被重新生成: %q", got.ServerID)
	}
}

func TestListenAddressValidation(t *testing.T) {
	ok := []string{"", "0.0.0.0", "127.0.0.1", "192.168.1.10"}
	for _, a := range ok {
		if err := ValidateListenAddress(a); err != nil {
			t.Fatalf("%q 应通过: %v", a, err)
		}
	}
	bad := []string{"192.168.1.10:18740", "1.2.3.4/24", "with space"}
	for _, a := range bad {
		if err := ValidateListenAddress(a); err == nil {
			t.Fatalf("%q 应被拒绝", a)
		}
	}
}

// v4 配置没有「跳过疑似验证码」字段，升级后必须默认为开：
// 否则用户升级完会发现验证码开始同步到手机，行为被悄悄改变。
func TestUpgradeDefaultsToSkipSuspectedOTP(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"version":4,"enabled":true,"listen_address":"0.0.0.0","port":18740,` +
		`"server_id":"` + NewUUID() + `","max_push_chars":5000}`)
	if err := os.WriteFile(filepath.Join(dir, FileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := Open(dir)
	if err == nil {
		t.Fatal("版本升级应产生修复提示")
	}
	got := store.Get()
	if !got.SkipSuspectedOTP {
		t.Fatal("旧配置升级后应默认跳过疑似验证码")
	}
	if got.Version != SchemaVersion {
		t.Fatalf("版本未升级: %d", got.Version)
	}
	if got.MaxPushChars != DefaultMaxPushChars {
		t.Fatalf("旧默认上限应跟随新默认 %d，实际 %d", DefaultMaxPushChars, got.MaxPushChars)
	}
	if got.ForcePushChars != DefaultForcePushChars {
		t.Fatalf("强制上限应为默认值 %d，实际 %d", DefaultForcePushChars, got.ForcePushChars)
	}
}

// v6 迁移只在用户没动过上限（仍是旧默认 5000）时才跟随新默认，
// 自定义值必须原样保留，否则会静默改掉用户的设置。
func TestUpgradeKeepsCustomPushLimit(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"version":5,"enabled":true,"listen_address":"0.0.0.0","port":18740,` +
		`"server_id":"` + NewUUID() + `","max_push_chars":8000,"skip_suspected_otp":true}`)
	if err := os.WriteFile(filepath.Join(dir, FileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := Open(dir)
	if err == nil {
		t.Fatal("版本升级应产生修复提示")
	}
	got := store.Get()
	if got.MaxPushChars != 8000 {
		t.Fatalf("自定义上限被覆盖: %d", got.MaxPushChars)
	}
	if got.ForcePushChars != DefaultForcePushChars {
		t.Fatalf("强制上限应为 %d，实际 %d", DefaultForcePushChars, got.ForcePushChars)
	}
}

// 默认配置必须两个上限都可用：普通 1000、强制 10000。
func TestDefaultPushLimits(t *testing.T) {
	cfg := Default()
	if cfg.MaxPushChars != 1000 {
		t.Fatalf("默认推送上限应为 1000，实际 %d", cfg.MaxPushChars)
	}
	if cfg.ForcePushChars != 10000 {
		t.Fatalf("默认强制上限应为 10000，实际 %d", cfg.ForcePushChars)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("默认配置应通过校验: %v", err)
	}
}

func TestForcePushCharsOutOfRange(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []int{0, -1, MaxPushChars + 1} {
		if _, err := store.Update(func(c *Config) error { c.ForcePushChars = v; return nil }); err == nil {
			t.Fatalf("强制上限 %d 应被拒绝", v)
		}
	}
	if store.Get().ForcePushChars != DefaultForcePushChars {
		t.Fatal("失败的更新不应改变内存配置")
	}
}

// 用户显式关闭后必须被尊重，重新读取不得被迁移逻辑改写回 true。
func TestSkipSuspectedOTPPersists(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(func(c *Config) error { c.SkipSuspectedOTP = false; return nil }); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	if reopened.Get().SkipSuspectedOTP {
		t.Fatal("用户关闭的选择不应被升级逻辑覆盖")
	}
}
