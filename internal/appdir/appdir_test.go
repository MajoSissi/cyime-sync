package appdir

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateFromCopiesConfigAndCert(t *testing.T) {
	root := t.TempDir()
	legacy := t.TempDir()
	writeFile(t, filepath.Join(legacy, ConfigFile), `{"version":2}`)
	writeFile(t, filepath.Join(legacy, TLSDirName, "cert.pem"), "CERT")
	writeFile(t, filepath.Join(legacy, TLSDirName, "key.pem"), "KEY")

	msg, err := migrateFrom(root, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if msg == "" {
		t.Fatal("应返回迁移说明")
	}
	for _, name := range []string{ConfigFile, filepath.Join(TLSDirName, "cert.pem"), filepath.Join(TLSDirName, "key.pem")} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("%s 未迁移: %v", name, err)
		}
	}
	// 旧目录必须原样保留，迁移不是移动。
	if _, err := os.Stat(filepath.Join(legacy, ConfigFile)); err != nil {
		t.Fatalf("旧配置不应被删除: %v", err)
	}
}

func TestMigrateFromSkipsWhenTargetExists(t *testing.T) {
	root := t.TempDir()
	legacy := t.TempDir()
	writeFile(t, filepath.Join(root, ConfigFile), `{"version":3}`)
	writeFile(t, filepath.Join(legacy, ConfigFile), `{"version":2}`)

	msg, err := MigrateLegacy(root)
	if err != nil {
		t.Fatal(err)
	}
	if msg != "" {
		t.Fatalf("目标已存在时不应迁移: %s", msg)
	}
	raw, err := os.ReadFile(filepath.Join(root, ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"version":3}` {
		t.Fatalf("已有配置被覆盖: %s", raw)
	}
}

func TestEnsureWritableCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	if err := ensureWritable(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("目录未创建: %v", err)
	}
	// 探测用的临时文件不应残留。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("目录应为空，实际 %d 项", len(entries))
	}
}
