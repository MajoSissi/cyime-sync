// Package appdir 决定运行期数据目录：固定为可执行文件同级的 data/。
//
// 绿色化部署是本项目的硬要求——配置、证书、日志都跟着 exe 走，
// 用户把整个目录复制到 U 盘或另一台机器仍然可用。
// 仅当 exe 所在目录不可写（例如放在 Program Files 且无权限）时，
// 才退化到用户配置目录，保证程序至少能启动。
package appdir

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	// DirName 数据目录名（与 exe 同级）。
	DirName = "data"

	// ConfigFile 配置文件名。
	ConfigFile = "config.json"
	// TLSDirName 自签证书目录名。
	TLSDirName = "tls"
	// LogDirName 日志目录名。
	LogDirName = "logs"

	// legacyDirName 旧版使用的 %APPDATA% 目录名，仅用于一次性迁移。
	legacyDirName = "CyIMESync"

	dirPerm  = 0o700
	filePerm = 0o600
)

// Root 返回数据目录绝对路径，并确保它存在且可写。
func Root() (string, error) {
	if exe, err := executablePath(); err == nil {
		dir := filepath.Join(filepath.Dir(exe), DirName)
		if err := ensureWritable(dir); err == nil {
			return dir, nil
		}
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("无法定位可写的数据目录: %w", err)
	}
	dir := filepath.Join(base, legacyDirName, DirName)
	if err := ensureWritable(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// Sub 返回数据目录下的子目录并确保存在。
func Sub(root, name string) (string, error) {
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return "", fmt.Errorf("创建目录 %s 失败: %w", dir, err)
	}
	return dir, nil
}

// executablePath 返回解析过符号链接的可执行文件路径。
func executablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// ensureWritable 创建目录并实际写入一个临时文件验证权限。
// 只判断 MkdirAll 成功是不够的：目录已存在但只读时仍然会失败。
func ensureWritable(dir string) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

// LegacyDirs 返回旧版数据目录候选（仅 Windows 的 %APPDATA%\CyIMESync）。
func LegacyDirs() []string {
	base, err := os.UserConfigDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(base, legacyDirName)}
}

// MigrateLegacy 把旧版数据复制到新目录，只在目标不存在时执行一次。
//
// 必须连证书一起搬：设备令牌与证书指纹绑定，只搬配置会导致手机端
// 全部需要重新扫码。旧目录保持原样，不做删除。
// 返回迁移说明；没有可迁移内容时返回空串。
func MigrateLegacy(root string) (string, error) {
	if _, err := os.Stat(filepath.Join(root, ConfigFile)); err == nil {
		return "", nil
	}
	for _, legacy := range LegacyDirs() {
		msg, err := migrateFrom(root, legacy)
		if err != nil || msg != "" {
			return msg, err
		}
	}
	return "", nil
}

// migrateFrom 从指定旧目录迁移，返回空串表示该目录没有可迁移内容。
func migrateFrom(root, legacy string) (string, error) {
	src := filepath.Join(legacy, ConfigFile)
	if _, err := os.Stat(src); err != nil {
		return "", nil
	}
	if err := copyFile(src, filepath.Join(root, ConfigFile)); err != nil {
		return "", fmt.Errorf("迁移配置失败: %w", err)
	}
	moved := "config.json"
	legacyTLS := filepath.Join(legacy, TLSDirName)
	if _, err := os.Stat(legacyTLS); err == nil {
		if err := copyTree(legacyTLS, filepath.Join(root, TLSDirName)); err != nil {
			return "", fmt.Errorf("迁移证书失败: %w", err)
		}
		moved += "、tls"
	}
	return fmt.Sprintf("已从 %s 迁移 %s", legacy, moved), nil
}

func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, dirPerm); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		from := filepath.Join(src, entry.Name())
		to := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := copyTree(from, to); err != nil {
				return err
			}
			continue
		}
		if !entry.Type().IsRegular() {
			continue
		}
		if err := copyFile(from, to); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, filePerm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
