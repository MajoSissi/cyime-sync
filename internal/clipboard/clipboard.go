// Package clipboard 抽象系统剪贴板的读写。
//
// 对外只暴露 Clipboard 接口与 Snapshot：Windows 实现放在 clipboard_windows.go，
// 测试与非 Windows 构建使用内存假实现，保证 go test / go vet 不依赖真实剪贴板。
package clipboard

import (
	"errors"
	"strings"
)

// Kind 描述剪贴板当前内容形态。
type Kind int

const (
	// KindEmpty 剪贴板为空（对应协议 204）。
	KindEmpty Kind = iota
	// KindText 剪贴板有纯文本。
	KindText
	// KindNonText 剪贴板是图片/文件/富文本等非纯文本内容（对应协议 204）。
	KindNonText
)

// String 便于日志输出。
func (k Kind) String() string {
	switch k {
	case KindText:
		return "text"
	case KindNonText:
		return "non-text"
	default:
		return "empty"
	}
}

// 剪贴板错误，服务层据此映射 409 / 503。
var (
	// ErrBusy 打开剪贴板重试耗尽（被其它进程占用）。
	ErrBusy = errors.New("clipboard busy")
	// ErrConflict 写入前序列号已变化（CAS 失败）。
	ErrConflict = errors.New("clipboard changed by another process")
	// ErrUnsupported 当前平台不支持。
	ErrUnsupported = errors.New("clipboard unsupported on this platform")
)

// SensitiveOrigin 说明敏感判定来自哪里，决定它能不能被用户设置覆盖。
type SensitiveOrigin int

const (
	// SensitiveNone 未判定为敏感。
	SensitiveNone SensitiveOrigin = iota
	// SensitiveDeclared 内容来源自己声明的隐私标记（密码管理器、系统"不监视/不进历史/不上云"）。
	// 这是来源的明确意图，不受设置影响，始终跳过同步。
	SensitiveDeclared
	// SensitiveGuess 本程序按内容形态猜测（4-8 位纯数字 ≈ 短信验证码）。
	// 只是猜测，可由设置项 skip_suspected_otp 关闭。
	SensitiveGuess
)

// Snapshot 是一次读取结果的快照。
type Snapshot struct {
	Kind Kind
	Text string
	// Seq 读取时的剪贴板序列号，写入 CAS 与自写抑制都依赖它。
	Seq uint32
	// Sensitive 表示该内容被判定为敏感（密码管理器标记、疑似验证码等），不宜外发。
	Sensitive bool
	// SensitiveReason 敏感判定原因，供事件流标注。
	SensitiveReason string
	// SensitiveOrigin 判定来源：来源声明不可关闭，本程序猜测可被用户关闭。
	SensitiveOrigin SensitiveOrigin
}

// Empty 返回空快照。
func Empty() Snapshot { return Snapshot{Kind: KindEmpty} }

// Clipboard 是剪贴板读写接口，所有方法都应在 500ms 预算内返回或报错。
type Clipboard interface {
	// Sequence 读取剪贴板序列号，廉价且不减量：只用于变化检测。
	Sequence() uint32
	// Read 读取当前内容；非文本返回 KindNonText 快照而非错误。
	Read() (Snapshot, error)
	// Write 在序列号等于 expectSeq 时写入文本，返回写入后的新序列号。
	// 序列号已变化返回 ErrConflict；无法打开返回 ErrBusy。
	Write(text string, expectSeq uint32) (uint32, error)
}

// isLikelyOTP 判定"疑似短信验证码"：4-8 位纯数字。
//
// 这只是形态猜测（订单号、工号、邮编都可能是纯数字），因此判定结果标记为
// SensitiveGuess，由用户在设置里决定是否沿用。默认跳过。
func isLikelyOTP(text string) bool {
	t := strings.TrimSpace(text)
	if len(t) < 4 || len(t) > 8 {
		return false
	}
	for i := 0; i < len(t); i++ {
		if t[i] < '0' || t[i] > '9' {
			return false
		}
	}
	return true
}
