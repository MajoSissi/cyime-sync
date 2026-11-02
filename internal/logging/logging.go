// Package logging 提供单文件、按大小轮转的日志输出，并定期清理超期文件。
//
// 不引入日志框架：只需要「写文件 + 满了换一个 + 删旧的」三件事，
// 标准库足够，也符合零重型依赖的约束。
package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultFileName 日志文件名。
	DefaultFileName = "cyime-sync.log"
	// DefaultMaxBytes 单个日志文件上限（1 MiB）。
	DefaultMaxBytes int64 = 1 << 20
	// DefaultMaxFiles 轮转文件总数（当前 1 个 + 历史 3 个），上限约 4 MiB。
	DefaultMaxFiles = 4
	// DefaultMaxAge 历史文件保留时长。
	DefaultMaxAge = 7 * 24 * time.Hour
	// DefaultCleanInterval 清理节奏：启动时一次，之后每 6 小时。
	DefaultCleanInterval = 6 * time.Hour

	dirPerm  = 0o700
	filePerm = 0o600
)

// Options 控制轮转策略，零值字段用默认值填充。
type Options struct {
	FileName string
	MaxBytes int64
	MaxFiles int
	MaxAge   time.Duration
}

// DefaultOptions 返回默认轮转策略。
func DefaultOptions() Options {
	return Options{
		FileName: DefaultFileName,
		MaxBytes: DefaultMaxBytes,
		MaxFiles: DefaultMaxFiles,
		MaxAge:   DefaultMaxAge,
	}
}

// Writer 是一个可并发写入的轮转文件写入器，可直接交给 log.SetOutput。
type Writer struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	maxFiles int
	maxAge   time.Duration

	file *os.File
	size int64
}

// Open 打开（或创建）日志目录下的日志文件，并清理一次超期历史。
func Open(dir string, opt Options) (*Writer, error) {
	if opt.FileName == "" {
		opt.FileName = DefaultFileName
	}
	if opt.MaxBytes <= 0 {
		opt.MaxBytes = DefaultMaxBytes
	}
	if opt.MaxFiles < 2 {
		opt.MaxFiles = DefaultMaxFiles
	}
	if opt.MaxAge <= 0 {
		opt.MaxAge = DefaultMaxAge
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("创建日志目录失败: %w", err)
	}
	w := &Writer{
		path:     filepath.Join(dir, opt.FileName),
		maxBytes: opt.MaxBytes,
		maxFiles: opt.MaxFiles,
		maxAge:   opt.MaxAge,
	}
	w.Cleanup()
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// Path 返回当前日志文件绝对路径。
func (w *Writer) Path() string { return w.path }

// Write 实现 io.Writer：写满上限前先轮转。
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	// size > 0 才轮转：单条超过上限的记录仍要完整写入，否则会丢掉关键报错。
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// Cleanup 删除超过保留期的历史文件，返回删除个数。
func (w *Writer) Cleanup() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cleanupLocked()
}

// Janitor 启动后台清理协程，返回停止函数（可安全重复调用）。
func (w *Writer) Janitor(interval time.Duration) func() {
	if interval <= 0 {
		interval = DefaultCleanInterval
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				w.Cleanup()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-done
		})
	}
}

// Close 关闭当前文件。
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// open 打开当前文件并记录已有大小（调用方需持锁或处于初始化阶段）。
func (w *Writer) open() error {
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, filePerm)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("读取日志文件大小失败: %w", err)
	}
	w.file = f
	w.size = st.Size()
	return nil
}

// rotate 把当前文件改名为 .1，历史文件依次后移，超出的最老文件删除。
func (w *Writer) rotate() error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	_ = os.Remove(w.backupPath(w.maxFiles - 1))
	for i := w.maxFiles - 2; i >= 1; i-- {
		_ = os.Rename(w.backupPath(i), w.backupPath(i+1))
	}
	if err := os.Rename(w.path, w.backupPath(1)); err != nil && !os.IsNotExist(err) {
		// 改名失败（例如文件被杀毒软件短暂占用）时尽力重新打开，
		// 让后续日志继续落到原文件，而不是从此写不进去。
		_ = w.open()
		return fmt.Errorf("轮转日志失败: %w", err)
	}
	return w.open()
}

// backupPath 返回第 i 份历史文件的路径（i 从 1 开始）。
func (w *Writer) backupPath(i int) string {
	ext := filepath.Ext(w.path)
	return fmt.Sprintf("%s.%d%s", strings.TrimSuffix(w.path, ext), i, ext)
}

func (w *Writer) cleanupLocked() int {
	dir := filepath.Dir(w.path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	// 只处理「当前文件名 + .序号 + 扩展名」的历史文件，绝不碰正在写的文件。
	ext := filepath.Ext(w.path)
	prefix := strings.TrimSuffix(filepath.Base(w.path), ext) + "."
	cutoff := time.Now().Add(-w.maxAge)
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		index := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), prefix), ext)
		if index == "" || strings.Trim(index, "0123456789") != "" {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err == nil {
			removed++
		}
	}
	return removed
}
