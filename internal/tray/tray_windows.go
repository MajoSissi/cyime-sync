//go:build windows

// Package tray 自绘 Windows 通知区域图标：注册窗口类、Shell_NotifyIconW、TrackPopupMenu。
//
// 不依赖 systray 类库，避免 CGO / 单文件 / 高 DPI 三类问题。
package tray

import (
	_ "embed"
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"

	"cyime-sync/internal/winutil"
)

//go:embed icon.ico
var iconData []byte

const (
	wmDestroy       = 0x0002
	wmClose         = 0x0010
	wmContext       = 0x007B
	wmNull          = 0x0000
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205

	nimAdd    = 0
	nimDelete = 2

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	smCXSmIcon = 49
	smCYSmIcon = 50

	mfString    = 0x00000000
	mfSeparator = 0x00000800
	mfDefault   = 0x00001000

	tpmRightButton = 0x0002
	tpmBottomAlign = 0x0020
	tpmNoNotify    = 0x0080
	tpmReturnCmd   = 0x0100

	cmdOpen = 1
	cmdPush = 2
	cmdQuit = 3

	iconResourceVersion = 0x00030000
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	// Shell_NotifyIconW 由 shell32.dll 导出，不是 user32。
	procShellNotifyIconW      = shell32.NewProc("Shell_NotifyIconW")
	procCreatePopupMenu       = user32.NewProc("CreatePopupMenu")
	procAppendMenuW           = user32.NewProc("AppendMenuW")
	procDestroyMenu           = user32.NewProc("DestroyMenu")
	procTrackPopupMenu        = user32.NewProc("TrackPopupMenu")
	procSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	procPostMessageW          = user32.NewProc("PostMessageW")
	procGetCursorPos          = user32.NewProc("GetCursorPos")
	procGetSystemMetrics      = user32.NewProc("GetSystemMetrics")
	procRegisterWindowMessage = user32.NewProc("RegisterWindowMessageW")
	procCreateIconFromRes     = user32.NewProc("CreateIconFromResourceEx")
	procGetModuleHandleW      = kernel32.NewProc("GetModuleHandleW")
)

// Options 是托盘的行为注入点，避免托盘包反向依赖配置与 HTTP 层。
type Options struct {
	// Tooltip 托盘悬停提示。
	Tooltip func() string
	// OnOpenSettings 双击托盘或点击菜单「设置」。
	OnOpenSettings func()
	// OnForcePush 手动推送当前剪贴板：忽略常规字符上限，改用手动推送上限。
	OnForcePush func()
}

// Run 创建托盘图标并进入消息循环，返回即表示用户选择退出。
// 必须在主 goroutine 调用，内部会 LockOSThread。
func Run(opts Options) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	app := &app{opts: opts}
	active = app

	hInst, _, _ := procGetModuleHandleW.Call(0)
	className, err := windows.UTF16PtrFromString(winutil.WindowClassName)
	if err != nil {
		return err
	}
	title, err := windows.UTF16PtrFromString("CyIME 剪贴板同步")
	if err != nil {
		return err
	}

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   callback,
		hInstance:     hInst,
		lpszClassName: className,
	}
	if r, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return fmt.Errorf("注册窗口类失败: %v", callErr)
	}

	// 创建但不显示窗口：仅为接收托盘消息与弹出菜单提供 HWND。
	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		0, 0, 0, 0, 0,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("创建托盘窗口失败: %v", callErr)
	}
	app.hwnd = hwnd

	msgName, _ := windows.UTF16PtrFromString("TaskbarCreated")
	app.taskbarCreated, _, _ = procRegisterWindowMessage.Call(uintptr(unsafe.Pointer(msgName)))

	app.icon = loadIcon(iconData, app.smallIconSize())
	if app.icon == 0 {
		return errors.New("托盘图标资源加载失败")
	}
	if err := app.addIcon(); err != nil {
		return err
	}
	defer app.removeIcon()

	app.runLoop()
	return nil
}

type point struct {
	x int32
	y int32
}

type msgT struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type notifyIconDataW struct {
	cbSize           uint32
	hwnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

type app struct {
	opts           Options
	hwnd           uintptr
	icon           uintptr
	ni             notifyIconDataW
	taskbarCreated uintptr
}

// active 指向当前唯一托盘实例（进程内只有一个窗口）。
var active *app

// callback 是交给 RegisterClassEx 的窗口过程，必须保持存活。
var callback = windows.NewCallback(wndProc)

func wndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	a := active
	if a != nil {
		switch msg {
		case winutil.WMAppOpenSettings:
			a.openSettings()
			return 0
		case winutil.WMAppTrayCallback:
			switch uint32(lParam) & 0xFFFF {
			case wmLButtonDblClk:
				a.openSettings()
			case wmRButtonUp, wmContext:
				a.showMenu()
			}
			return 0
		case wmClose:
			procDestroyWindow.Call(hwnd)
			return 0
		case wmDestroy:
			a.removeIcon()
			procPostQuitMessage.Call(0)
			return 0
		default:
			if a.taskbarCreated != 0 && uintptr(msg) == a.taskbarCreated {
				// 资源管理器重启后图标会消失，需要重新注册。
				_ = a.addIcon()
				return 0
			}
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return r
}

func (a *app) runLoop() {
	var m msgT
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // 0 = WM_QUIT，-1 = 错误
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func (a *app) smallIconSize() int {
	r, _, _ := procGetSystemMetrics.Call(smCXSmIcon)
	if r == 0 {
		return 16
	}
	return int(r)
}

func (a *app) tooltip() string {
	if a.opts.Tooltip != nil {
		return a.opts.Tooltip()
	}
	return "CyIME 剪贴板同步"
}

func (a *app) addIcon() error {
	a.ni = notifyIconDataW{
		hwnd:             a.hwnd,
		uID:              1,
		uFlags:           nifMessage | nifIcon | nifTip,
		uCallbackMessage: winutil.WMAppTrayCallback,
		hIcon:            a.icon,
	}
	a.ni.cbSize = uint32(unsafe.Sizeof(a.ni))
	copyTip(a.ni.szTip[:], a.tooltip())

	if r, _, _ := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&a.ni))); r == 0 {
		return errors.New("Shell_NotifyIcon 添加图标失败")
	}
	return nil
}

func (a *app) removeIcon() {
	if a.hwnd == 0 {
		return
	}
	ni := notifyIconDataW{hwnd: a.hwnd, uID: 1}
	ni.cbSize = uint32(unsafe.Sizeof(ni))
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&ni)))
}

func (a *app) openSettings() {
	if a.opts.OnOpenSettings != nil {
		a.opts.OnOpenSettings()
	}
}

// menuItem 描述一个右键菜单项。单独抽成数据，便于测试菜单结构
// （菜单只能靠人手点，结构写错很难在自动验收里发现）。
type menuItem struct {
	id    int
	flags uintptr
	text  string
}

func menuItems() []menuItem {
	return []menuItem{
		{cmdOpen, mfString | mfDefault, "设置"},
		{cmdPush, mfString, "推送"},
		{0, mfSeparator, ""},
		{cmdQuit, mfString, "退出"},
	}
}

func (a *app) showMenu() {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	for _, it := range menuItems() {
		appendMenu(menu, it.flags, it.id, it.text)
	}

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(a.hwnd)

	cmd, _, _ := procTrackPopupMenu.Call(menu,
		tpmRightButton|tpmBottomAlign|tpmNoNotify|tpmReturnCmd,
		uintptr(int(pt.x)), uintptr(int(pt.y)), 0, a.hwnd, 0)
	// 菜单关闭后向窗口投递空消息，确保下次点击仍能正常弹出。
	procPostMessageW.Call(a.hwnd, wmNull, 0, 0)

	switch uint32(cmd) {
	case cmdOpen:
		a.openSettings()
	case cmdPush:
		if a.opts.OnForcePush != nil {
			a.opts.OnForcePush()
		}
	case cmdQuit:
		procDestroyWindow.Call(a.hwnd)
	}
}

func appendMenu(menu uintptr, flags uintptr, id int, text string) {
	var p uintptr
	if text != "" {
		if s, err := windows.UTF16PtrFromString(text); err == nil {
			p = uintptr(unsafe.Pointer(s))
		}
	}
	procAppendMenuW.Call(menu, flags, uintptr(id), p)
}

// copyTip 把 Go 字符串写入固定长度 UTF-16 字段（超长截断并保证 NUL 结尾）。
func copyTip(dst []uint16, s string) {
	if len(dst) == 0 {
		return
	}
	u, err := windows.UTF16FromString(s)
	if err != nil || len(u) == 0 {
		u = []uint16{0}
	}
	if len(u) > len(dst) {
		u = append(u[:len(dst)-1:len(dst)-1], 0)
	}
	copy(dst, u)
}

// loadIcon 从内存中的 ICO 资源里挑选最接近目标尺寸的帧并生成 HICON。
func loadIcon(data []byte, want int) uintptr {
	if len(data) < 6 {
		return 0
	}
	count := int(data[4]) | int(data[5])<<8
	best, bestDiff := -1, 1<<30
	bestSize := 0
	for i := 0; i < count; i++ {
		off := 6 + i*16
		if off+16 > len(data) {
			break
		}
		w := int(data[off])
		if w == 0 {
			w = 256
		}
		diff := w - want
		if diff < 0 {
			diff = -diff
		}
		if diff < bestDiff {
			best, bestDiff, bestSize = i, diff, w
		}
	}
	if best < 0 {
		return 0
	}
	off := 6 + best*16
	size := int(uint32(data[off+8]) | uint32(data[off+9])<<8 | uint32(data[off+10])<<16 | uint32(data[off+11])<<24)
	imgOff := int(uint32(data[off+12]) | uint32(data[off+13])<<8 | uint32(data[off+14])<<16 | uint32(data[off+15])<<24)
	if size <= 0 || imgOff+size > len(data) {
		return 0
	}
	h, _, _ := procCreateIconFromRes.Call(
		uintptr(unsafe.Pointer(&data[imgOff])),
		uintptr(size),
		1,
		iconResourceVersion,
		uintptr(bestSize),
		uintptr(bestSize),
		0,
	)
	return h
}
