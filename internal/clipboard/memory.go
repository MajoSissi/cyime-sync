package clipboard

import (
	"sync"
	"sync/atomic"
)

// Memory 是内存假实现，用于单元测试与开发期无剪贴板环境。
//
// 行为刻意贴近真实实现：每次变更递增序列号；写入时校验期望序列号。
type Memory struct {
	mu   sync.Mutex
	seq  uint32
	snap Snapshot
	// Sensitive 置位后下一次 Read 返回敏感快照（不影响真实内容）。
	Sensitive bool
	Reason    string
	// Origin 是敏感判定来源，默认 SensitiveDeclared（来源声明，不可关闭）。
	Origin SensitiveOrigin
	// Busy 置位后所有操作返回 ErrBusy，用于模拟 503。
	Busy atomic.Bool
}

// NewMemory 创建带初始文本的内存剪贴板。
func NewMemory(initial string) *Memory {
	m := &Memory{seq: 1}
	if initial != "" {
		m.snap = Snapshot{Kind: KindText, Text: initial, Seq: m.seq}
	} else {
		m.snap = Snapshot{Kind: KindEmpty, Seq: m.seq}
	}
	return m
}

// Sequence 实现 Clipboard。
func (m *Memory) Sequence() uint32 {
	return atomic.LoadUint32(&m.seq)
}

// Read 实现 Clipboard。
func (m *Memory) Read() (Snapshot, error) {
	if m.Busy.Load() {
		return Snapshot{}, ErrBusy
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.snap
	s.Seq = m.seq
	if m.Sensitive {
		s.Sensitive = true
		s.SensitiveReason = m.Reason
		s.SensitiveOrigin = m.Origin
		if s.SensitiveOrigin == SensitiveNone {
			s.SensitiveOrigin = SensitiveDeclared
		}
	}
	return s, nil
}

// Write 实现 Clipboard。
func (m *Memory) Write(text string, expectSeq uint32) (uint32, error) {
	if m.Busy.Load() {
		return 0, ErrBusy
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if expectSeq != 0 && expectSeq != m.seq {
		return 0, ErrConflict
	}
	next := atomic.AddUint32(&m.seq, 1)
	if text == "" {
		m.snap = Snapshot{Kind: KindEmpty, Seq: next}
	} else {
		m.snap = Snapshot{Kind: KindText, Text: text, Seq: next}
	}
	// 真实剪贴板写入后敏感标记随之消失。
	m.Sensitive = false
	m.Reason = ""
	return next, nil
}

// SetNonText 模拟复制了图片/文件，序列号同样递增。
func (m *Memory) SetNonText() {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := atomic.AddUint32(&m.seq, 1)
	m.snap = Snapshot{Kind: KindNonText, Seq: next}
}

// Clear 模拟清空剪贴板。
func (m *Memory) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := atomic.AddUint32(&m.seq, 1)
	m.snap = Snapshot{Kind: KindEmpty, Seq: next}
}

// SetSensitive 让后续读取被判定为"来源声明的敏感内容"（不受设置影响）。
func (m *Memory) SetSensitive(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Sensitive = true
	m.Reason = reason
	m.Origin = SensitiveDeclared
}

// SetGuessSensitive 让后续读取被判定为"疑似验证码"（可被设置关闭）。
func (m *Memory) SetGuessSensitive(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Sensitive = true
	m.Reason = reason
	m.Origin = SensitiveGuess
}

// 编译期断言：Memory 满足 Clipboard。
var _ Clipboard = (*Memory)(nil)
