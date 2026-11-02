//go:build !windows

package winutil

import "errors"

// 非 Windows 平台仅提供同名占位实现，保证 go vet ./... 与 go test ./... 可以通过。

const (
	WindowClassName   = "CyIMESync.TrayWindow"
	WMAppTrayCallback = 0x8000 + 1
	WMAppOpenSettings = 0x8000 + 2
	RunKey            = `Software\Microsoft\Windows\CurrentVersion\Run`
	RunValueName      = "CyIMESync"
)

// ErrUnsupported 表示当前平台不具备该 Win32 能力。
var ErrUnsupported = errors.New("win32 only")

// SetProcessDpiAware 空实现。
func SetProcessDpiAware() {}

// AcquireSingleInstance 非 Windows 上恒返回 false（不限制多实例）。
func AcquireSingleInstance() (bool, error) { return false, nil }

// NotifyExistingInstance 非 Windows 上无操作。
func NotifyExistingInstance() error { return ErrUnsupported }

// OpenURL 非 Windows 上无操作。
func OpenURL(string) error { return ErrUnsupported }

// MessageBox 非 Windows 上无操作。
func MessageBox(string, string) {}

// SetAutoStart 非 Windows 上无操作。
func SetAutoStart(bool) error { return ErrUnsupported }
