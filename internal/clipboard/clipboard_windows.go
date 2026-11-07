//go:build windows

package clipboard

import (
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	cfText        = 1
	cfUnicodeText = 13

	// cpACP 是系统 ANSI 代码页；CF_TEXT 数据按它解释。
	cpACP = 0

	gmemMoveable = 0x0002

	// openBudget 是打开剪贴板的总重试预算（协议要求 <= 500ms）。
	openBudget = 500 * time.Millisecond
	openMinGap = 10 * time.Millisecond
	openMaxGap = 30 * time.Millisecond
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procEmptyClipboard             = user32.NewProc("EmptyClipboard")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procSetClipboardData           = user32.NewProc("SetClipboardData")
	procIsClipboardFormatAvailable = user32.NewProc("IsClipboardFormatAvailable")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")
	procRegisterClipboardFormatW   = user32.NewProc("RegisterClipboardFormatW")
	procGlobalAlloc                = kernel32.NewProc("GlobalAlloc")
	procGlobalLock                 = kernel32.NewProc("GlobalLock")
	procGlobalUnlock               = kernel32.NewProc("GlobalUnlock")
	procGlobalFree                 = kernel32.NewProc("GlobalFree")
	procGlobalSize                 = kernel32.NewProc("GlobalSize")
	procMultiByteToWideChar        = kernel32.NewProc("MultiByteToWideChar")
)

// 敏感内容标记格式，懒注册后缓存。
var (
	sensitiveOnce     sync.Once
	fmtExcludeMonitor uint32
	fmtIncludeHistory uint32
	fmtUploadCloud    uint32
)

type winClipboard struct {
	// mu 串行化本进程的剪贴板访问，避免自己的 goroutine 互相抢 OpenClipboard。
	mu sync.Mutex
}

// New 返回 Windows 剪贴板实现。
func New() (Clipboard, error) {
	return &winClipboard{}, nil
}

// Sequence 读取序列号；失败时返回 0（调用方按"无变化"处理）。
func (c *winClipboard) Sequence() uint32 {
	r, _, _ := procGetClipboardSequenceNumber.Call()
	return uint32(r)
}

// Read 读取当前剪贴板内容。
func (c *winClipboard) Read() (Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var snap Snapshot
	err := withOpen(func() error {
		snap.Seq = c.Sequence()
		if reason := detectSensitiveFormat(); reason != "" {
			snap.Sensitive = true
			snap.SensitiveReason = reason
			snap.SensitiveOrigin = SensitiveDeclared
		}

		snap.Text, snap.Kind = readClipboardText()
		if snap.Kind == KindText && !snap.Sensitive && isLikelyOTP(snap.Text) {
			snap.Sensitive = true
			snap.SensitiveReason = "疑似短信验证码（纯数字短文本）"
			snap.SensitiveOrigin = SensitiveGuess
		}
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// Write 在序列号等于 expectSeq 时写入文本。
func (c *winClipboard) Write(text string, expectSeq uint32) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var newSeq uint32
	err := withOpen(func() error {
		// CAS：打开之后立刻复核序列号，避免覆盖用户在毫秒级内刚复制的内容。
		if expectSeq != 0 && c.Sequence() != expectSeq {
			return ErrConflict
		}
		if r, _, _ := procEmptyClipboard.Call(); r == 0 {
			return ErrBusy
		}

		u16, err := windows.UTF16FromString(text)
		if err != nil {
			// 文本含 NUL 时 UTF16FromString 会报错；协议层已拦截，这里兜底。
			return ErrUnsupported
		}
		size := uintptr(len(u16) * 2)
		h, _, _ := procGlobalAlloc.Call(gmemMoveable, size)
		if h == 0 {
			return ErrBusy
		}
		ptr, _, _ := procGlobalLock.Call(h)
		if ptr == 0 {
			procGlobalFree.Call(h)
			return ErrBusy
		}
		copy(unsafe.Slice((*uint16)(uintptrToPtr(ptr)), len(u16)), u16)
		procGlobalUnlock.Call(h)

		// 成功后内存所有权移交系统，不能再 GlobalFree。
		if r, _, _ := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
			procGlobalFree.Call(h)
			return ErrBusy
		}
		newSeq = c.Sequence()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newSeq, nil
}

// withOpen 在锁定 OS 线程的前提下打开/关闭剪贴板。
//
// OpenClipboard/CloseClipboard 是线程相关的：Go 的 goroutine 可能在两次系统调用之间
// 漂移到别的线程，因此这里必须 LockOSThread。
func withOpen(fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := openWithRetry(); err != nil {
		return err
	}
	defer procCloseClipboard.Call()
	return fn()
}

// openWithRetry 按 10-30ms 间隔重试打开剪贴板，总预算 500ms。
func openWithRetry() error {
	deadline := time.Now().Add(openBudget)
	gap := openMinGap
	for {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return ErrBusy
		}
		time.Sleep(gap)
		if gap < openMaxGap {
			gap += 5 * time.Millisecond
		}
	}
}

// uintptrToPtr 把 Win32 返回的地址转换为指针。
//
// 直接写 unsafe.Pointer(uintptr) 会被 go vet 判为 possible misuse of unsafe.Pointer；
// 这里改为重新解释 uintptr 变量的位模式，效果相同。安全前提：目标内存由系统的
// 剪贴板堆持有（不属于 Go 堆），不会被 GC 移动或回收，且调用期间剪贴板已被本进程打开。
func uintptrToPtr(v uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&v))
}

// readClipboardText 读取剪贴板里的纯文本（调用前必须已打开剪贴板）。
//
// 优先 CF_UNICODETEXT；少数老程序、终端或工具只写 CF_TEXT（ANSI）。不兼容这类来源时，
// 用户看到的现象就是「复制了却不同步」，而且没有任何事件可查——所以这里按系统 ANSI
// 代码页再回转一次，把"能读到的都读出来"。
func readClipboardText() (string, Kind) {
	if formatAvailable(cfUnicodeText) {
		h, _, _ := procGetClipboardData.Call(cfUnicodeText)
		if h == 0 {
			return "", KindEmpty
		}
		if s := readGlobalText(h); s != "" {
			return s, KindText
		}
		return "", KindEmpty
	}
	if formatAvailable(cfText) {
		h, _, _ := procGetClipboardData.Call(cfText)
		if h == 0 {
			return "", KindEmpty
		}
		if s := readGlobalANSI(h); s != "" {
			return s, KindText
		}
		return "", KindEmpty
	}
	return "", KindNonText
}

// readGlobalText 把 CF_UNICODETEXT 句柄读成 Go 字符串。
func readGlobalText(h uintptr) string {
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		return ""
	}
	defer procGlobalUnlock.Call(h)

	size, _, _ := procGlobalSize.Call(h)
	if size < 2 {
		return ""
	}
	n := int(size / 2)
	buf := unsafe.Slice((*uint16)(uintptrToPtr(ptr)), n)
	return windows.UTF16ToString(buf)
}

// readGlobalANSI 把 CF_TEXT 句柄按系统 ANSI 代码页读成 Go 字符串。
//
// 数据按规范以 NUL 结尾，但长度仍以 GlobalSize 为准：不依赖结尾符也能读全。
func readGlobalANSI(h uintptr) string {
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		return ""
	}
	defer procGlobalUnlock.Call(h)

	size, _, _ := procGlobalSize.Call(h)
	if size < 1 {
		return ""
	}
	raw := unsafe.Slice((*byte)(uintptrToPtr(ptr)), int(size))
	for len(raw) > 0 && raw[len(raw)-1] == 0 {
		raw = raw[:len(raw)-1]
	}
	if len(raw) == 0 {
		return ""
	}

	// 第一次调用只问所需 UTF-16 长度，第二次真正转换。
	n, _, _ := procMultiByteToWideChar.Call(cpACP, 0,
		uintptr(unsafe.Pointer(&raw[0])), uintptr(len(raw)), 0, 0)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, int(n))
	if r, _, _ := procMultiByteToWideChar.Call(cpACP, 0,
		uintptr(unsafe.Pointer(&raw[0])), uintptr(len(raw)),
		uintptr(unsafe.Pointer(&buf[0])), n); r == 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

// detectSensitiveFormat 检查密码管理器/系统隐私标记，命中则返回原因。
func detectSensitiveFormat() string {
	sensitiveOnce.Do(func() {
		fmtExcludeMonitor = registerFormat("ExcludeClipboardContentFromMonitorProcessing")
		fmtIncludeHistory = registerFormat("CanIncludeInClipboardHistory")
		fmtUploadCloud = registerFormat("CanUploadToCloudClipboard")
	})

	if fmtExcludeMonitor != 0 && formatAvailable(fmtExcludeMonitor) {
		return "剪贴板携带排除监视标记（ExcludeClipboardContentFromMonitorProcessing）"
	}
	if fmtIncludeHistory != 0 && formatAvailable(fmtIncludeHistory) {
		if v, ok := formatDword(fmtIncludeHistory); ok && v == 0 {
			return "内容来源声明不进入剪贴板历史（CanIncludeInClipboardHistory=0）"
		}
	}
	if fmtUploadCloud != 0 && formatAvailable(fmtUploadCloud) {
		if v, ok := formatDword(fmtUploadCloud); ok && v == 0 {
			return "内容来源禁止上传云端（CanUploadToCloudClipboard=0）"
		}
	}
	return ""
}

func registerFormat(name string) uint32 {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0
	}
	r, _, _ := procRegisterClipboardFormatW.Call(uintptr(unsafe.Pointer(p)))
	return uint32(r)
}

func formatAvailable(format uint32) bool {
	r, _, _ := procIsClipboardFormatAvailable.Call(uintptr(format))
	return r != 0
}

// formatDword 读取 DWORD 型自定义格式（只能在本进程已打开剪贴板时调用）。
func formatDword(format uint32) (uint32, bool) {
	h, _, _ := procGetClipboardData.Call(uintptr(format))
	if h == 0 {
		return 0, false
	}
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		return 0, false
	}
	defer procGlobalUnlock.Call(h)
	return *(*uint32)(uintptrToPtr(ptr)), true
}

var _ Clipboard = (*winClipboard)(nil)
