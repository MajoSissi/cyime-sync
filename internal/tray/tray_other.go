//go:build !windows

package tray

import "errors"

// Options 与 Windows 版本保持同一形状，便于 main 在任意平台编译。
type Options struct {
	Tooltip        func() string
	OnOpenSettings func()
	OnForcePush    func()
}

// Run 在非 Windows 平台直接返回错误。
func Run(Options) error {
	return errors.New("通知区域图标仅支持 Windows")
}
