//go:build windows

// Package winutil 收敛所有非剪贴板的 Win32 依赖：懒加载绑定、单实例、自启动、DPI、打开浏览器。
//
// 只依赖 golang.org/x/sys/windows 与 syscall.NewLazyDLL，不引入任何 CGO。
package winutil

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	// WindowClassName 托盘窗口类名。第二个实例靠它定位已有窗口。
	WindowClassName = "CyIMESync.TrayWindow"

	// WMAppTrayCallback 托盘图标回调消息（WM_APP + 1）。
	WMAppTrayCallback = 0x8000 + 1
	// WMAppOpenSettings 请求已运行实例打开设置页（WM_APP + 2）。
	WMAppOpenSettings = 0x8000 + 2

	// RunKey 开机自启动注册表键。
	RunKey = `Software\Microsoft\Windows\CurrentVersion\Run`
	// RunValueName 开机自启动值名。
	RunValueName = "CyIMESync"

	mutexName = `Global\CyIMESync.SingleInstance`

	swShowNormal = 1
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")

	procFindWindowW                   = user32.NewProc("FindWindowW")
	procPostMessageW                  = user32.NewProc("PostMessageW")
	procMessageBoxW                   = user32.NewProc("MessageBoxW")
	procSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procShellExecuteW                 = shell32.NewProc("ShellExecuteW")
	procGetModuleHandleW              = kernel32.NewProc("GetModuleHandleW")

	// 单实例互斥量句柄。必须活到进程退出，故放在包级变量并禁止 GC 回收。
	mutexHandle windows.Handle
)

// SetProcessDpiAware 声明 Per-Monitor V2 DPI 感知，避免托盘图标在高分屏被位图拉伸。
// 必须在创建任何窗口前调用。
func SetProcessDpiAware() {
	const dpiAwarenessPerMonitorV2 = ^uintptr(3) // -4
	if r, _, _ := procSetProcessDpiAwarenessContext.Call(dpiAwarenessPerMonitorV2); r != 0 {
		return
	}
	procSetProcessDPIAware.Call()
}

// AcquireSingleInstance 创建命名互斥量。
// 返回 alreadyRunning=true 表示已有实例在运行，调用方应通知它并退出。
func AcquireSingleInstance() (alreadyRunning bool, err error) {
	name, err := windows.UTF16PtrFromString(mutexName)
	if err != nil {
		return false, err
	}
	h, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return true, nil
		}
		return false, fmt.Errorf("创建单实例互斥量失败: %w", err)
	}
	mutexHandle = h
	return false, nil
}

// NotifyExistingInstance 向已运行实例的托盘窗口投递"打开设置页"消息。
func NotifyExistingInstance() error {
	cls, err := windows.UTF16PtrFromString(WindowClassName)
	if err != nil {
		return err
	}
	hwnd, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(cls)), 0)
	if hwnd == 0 {
		return errors.New("未找到已运行实例的窗口")
	}
	procPostMessageW.Call(hwnd, WMAppOpenSettings, 0, 0)
	return nil
}

// OpenURL 用系统默认浏览器打开地址。
func OpenURL(url string) error {
	verb, _ := windows.UTF16PtrFromString("open")
	target, err := windows.UTF16PtrFromString(url)
	if err != nil {
		return err
	}
	r, _, callErr := procShellExecuteW.Call(0,
		uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(target)), 0, 0, swShowNormal)
	// ShellExecuteW 返回值 > 32 表示成功。
	if r <= 32 {
		if callErr != nil && callErr != windows.ERROR_SUCCESS {
			return fmt.Errorf("打开浏览器失败: %v", callErr)
		}
		return fmt.Errorf("打开浏览器失败（ShellExecute 返回 %d）", r)
	}
	return nil
}

// MessageBox 在图形界面下弹出提示（windowsgui 子系统没有控制台）。
func MessageBox(title, text string) {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(text)
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x00000010)
}

// SetAutoStart 写入/删除 HKCU Run 项。注册的是带引号的完整路径，兼容含空格目录。
func SetAutoStart(enabled bool) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, RunKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表自启动键失败: %w", err)
	}
	defer key.Close()

	if !enabled {
		if err := key.DeleteValue(RunValueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("移除自启动项失败: %w", err)
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取可执行文件路径失败: %w", err)
	}
	cmd := `"` + exe + `"`
	if err := key.SetStringValue(RunValueName, cmd); err != nil {
		return fmt.Errorf("写入自启动项失败: %w", err)
	}
	return nil
}
