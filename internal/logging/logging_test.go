package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func listLogs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestRotatesBySizeAndKeepsLimit(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, Options{MaxBytes: 256, MaxFiles: 3, MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if _, err := fmt.Fprintf(w, "第 %02d 行：0123456789abcdef\n", i); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	logs := listLogs(t, dir)
	if len(logs) != 3 {
		t.Fatalf("应只保留 3 个日志文件，实际 %d：%v", len(logs), logs)
	}
	for _, name := range logs {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > 300 {
			t.Fatalf("%s 超过大小上限：%d 字节", name, info.Size())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, DefaultFileName)); err != nil {
		t.Fatalf("当前日志文件应存在: %v", err)
	}
}

func TestCleanupRemovesExpiredBackups(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "cyime-sync.1.log")
	if err := os.WriteFile(old, []byte("旧日志\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, "cyime-sync.2.log")
	if err := os.WriteFile(fresh, []byte("新日志\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	w, err := Open(dir, Options{MaxBytes: 1024, MaxFiles: 4, MaxAge: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("超过保留期的历史文件应被删除")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("未超期的历史文件不应被删除: %v", err)
	}
}

// 当前正在写的文件永远不能被清理逻辑误删。
func TestCleanupKeepsActiveFile(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, Options{MaxBytes: 1024, MaxFiles: 4, MaxAge: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := fmt.Fprint(w, "运行中\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	w.Cleanup()

	if _, err := os.Stat(filepath.Join(dir, DefaultFileName)); err != nil {
		t.Fatalf("当前日志文件被误删: %v", err)
	}
}

func TestFileNameSanitizedPrefix(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, Options{FileName: "app.log", MaxBytes: 128, MaxFiles: 2, MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		fmt.Fprintf(w, "0123456789abcdef\n")
	}
	w.Close()
	for _, name := range listLogs(t, dir) {
		if !strings.HasPrefix(name, "app.") {
			t.Fatalf("文件名不符: %s", name)
		}
	}
}
