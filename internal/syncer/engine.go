// Package syncer 实现双向同步引擎：变化检测、自写抑制、冲突处理与事件流。
//
// 关键约束：
//   - 变化检测只依赖 GetClipboardSequenceNumber，序列号变化才真正读取内容；
//   - 本进程写入引起的序列号变化不得回送成"电脑有新内容"事件；
//   - 重复推送同一内容必须按成功处理，且不重复写剪贴板；
//   - 敏感内容不上报、不入事件预览，只在事件里标注原因。
package syncer

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"cyime-sync/internal/clipboard"
	"cyime-sync/internal/protocol"
)

const (
	// PollInterval 序列号轮询间隔。安卓端约 2 秒 GET 一次，350ms 足够新鲜。
	PollInterval = 350 * time.Millisecond
	// PreviewLimit 正文预览最大字符数（只在内存里，不落盘）。
	PreviewLimit = 160
	// previewHead / previewTail 超长预览保留的头尾长度，中间用省略号。
	previewHead = 120
	previewTail = 40
)

// Engine 是同步引擎，可被 HTTP 处理器与托盘并发调用。
type Engine struct {
	clip clipboard.Clipboard
	ring *Ring

	// pushLimit 由外部注入，返回当前允许推送的最大字符数（<=0 表示不限制）。
	pushLimit func() int
	// forceLimit 由外部注入，返回手动强制推送允许的最大字符数（<=0 表示不限制）。
	forceLimit func() int
	// skipGuess 由外部注入，返回是否跳过"疑似验证码"这类形态猜测。
	skipGuess func() bool

	mu      sync.RWMutex
	kind    clipboard.Kind
	text    string
	etag    string
	lastSeq uint32
	selfSeq uint32
	// forceHash 是最近一次手动推送成功的内容 hash。同一段内容再被观测到时继续放行：
	// 序列号会因「其它程序重设了同一段内容」而变化，不能因此把用户刚强推的内容撤下。
	forceHash string
	// seenHash / seenStatus 是上一次观测的内容与判定结果，用于跨轮次去重：
	// 序列号变化但内容和结论都没变时，不重复记事件。
	seenHash   string
	seenStatus string

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
	enabled   atomic.Bool
}

// New 创建引擎。
func New(clip clipboard.Clipboard, ring *Ring) *Engine {
	e := &Engine{
		clip:       clip,
		ring:       ring,
		pushLimit:  func() int { return 0 },
		forceLimit: func() int { return 0 },
		skipGuess:  func() bool { return true },
		kind:       clipboard.KindEmpty,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	e.enabled.Store(true)
	return e
}

// SetEnabled 控制是否监听剪贴板变化。停用时不产生事件，接口层返回 503。
func (e *Engine) SetEnabled(v bool) { e.enabled.Store(v) }

// SetPushLimitFunc 注入推送字符上限读取函数（每次判定时实时读取，改设置立即生效）。
func (e *Engine) SetPushLimitFunc(fn func() int) {
	if fn != nil {
		e.pushLimit = fn
	}
}

// SetForceLimitFunc 注入强制推送的字符上限读取函数。
func (e *Engine) SetForceLimitFunc(fn func() int) {
	if fn != nil {
		e.forceLimit = fn
	}
}

// SetSkipGuessFunc 注入"跳过疑似验证码"开关（每次判定实时读取）。
func (e *Engine) SetSkipGuessFunc(fn func() bool) {
	if fn != nil {
		e.skipGuess = fn
	}
}

// shouldSkip 判断本次读取是否跳过同步。
//
// 内容来源自己声明的隐私标记（密码管理器、系统禁止监视/上云）一律跳过：那是来源的
// 明确意图，用户开关无权推翻。只有本程序按形态猜测的"疑似验证码"才交给用户决定——
// 纯数字短文本也可能是订单号、工号、邮编，一刀切跳过会让用户困惑。
func (e *Engine) shouldSkip(snap clipboard.Snapshot) bool {
	if !snap.Sensitive {
		return false
	}
	if snap.SensitiveOrigin == clipboard.SensitiveGuess {
		return e.skipGuess()
	}
	return true
}

// Ring 暴露事件环，供 HTTP 层订阅与清空。
func (e *Engine) Ring() *Ring { return e.ring }

// Start 做一次预热读取，然后启动序列号监听协程。
func (e *Engine) Start() {
	e.startOnce.Do(func() {
		e.refresh(true)
		go e.watch()
	})
}

// Stop 停止监听协程。
func (e *Engine) Stop() {
	e.stopOnce.Do(func() {
		close(e.stop)
		<-e.done
	})
}

// Refresh 立即重新读取剪贴板并同步缓存（启动预热与测试用）。
func (e *Engine) Refresh() { e.refresh(false) }

// Current 返回缓存的当前内容与 ETag。kind 为 KindText 时 text 才有意义。
func (e *Engine) Current() (text, etag string, kind clipboard.Kind) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.text, e.etag, e.kind
}

// SequenceInfo 返回最后一次观测到的序列号，用于状态页排障。
func (e *Engine) SequenceInfo() uint32 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastSeq
}

// Result 描述一次写入的结果。
type Result struct {
	ETag   string
	Status string
}

// ErrDisabled 表示同步已停用：托盘与接口层据此提示用户先启用。
var ErrDisabled = errors.New("同步已停用")

// ForcePush 立即按「手动推送上限」推送一次当前剪贴板，忽略常规推送上限。
//
// 只放宽长度限制：敏感内容（密码管理器声明、疑似验证码）仍然跳过。强制推送表达的是
// "我现在就要发这一段"，不是"允许把隐私内容发出去"。
func (e *Engine) ForcePush() (Result, error) {
	if !e.enabled.Load() {
		e.record(Event{Direction: DirOut}, StatusInvalid, "同步已停用，未推送")
		return Result{}, ErrDisabled
	}
	snap, err := e.clip.Read()
	if err != nil {
		e.record(Event{Direction: DirOut}, StatusBusy, "读取剪贴板失败")
		return Result{Status: StatusBusy}, clipboard.ErrBusy
	}
	if snap.Seq == 0 {
		snap.Seq = e.clip.Sequence()
	}
	meta := Event{Direction: DirOut, Source: protocol.SourceDesktop, Bytes: len(snap.Text)}

	switch {
	case e.shouldSkip(snap):
		e.clearCache(snap.Seq)
		e.record(meta, StatusSensitive, "已跳过同步："+snap.SensitiveReason)
		return Result{Status: StatusSensitive}, nil
	case snap.Kind != clipboard.KindText:
		e.clearCache(snap.Seq)
		e.record(meta, StatusEmpty, "剪贴板不是文本，没有可推送的内容")
		return Result{Status: StatusEmpty}, nil
	}

	limit := e.forceLimit()
	if note := e.oversizeNote(limit, snap.Text, true); note != "" {
		e.clearCache(snap.Seq)
		meta.Preview = previewText(snap.Text)
		e.record(meta, StatusOversize, note)
		return Result{Status: StatusOversize}, nil
	}

	etag := e.forceCommit(snap.Seq, snap.Text)
	meta.Preview = previewText(snap.Text)
	e.record(meta, StatusSynced, "已手动推送")
	return Result{ETag: etag, Status: StatusSynced}, nil
}

// Apply 处理手机 PUT：写入剪贴板并抑制自写回环。
//
// 返回的 error 只可能是 clipboard.ErrBusy / clipboard.ErrConflict，
// 由 HTTP 层映射为 503 / 409。
func (e *Engine) Apply(text, source string) (Result, error) {
	size, hash := protocol.Digest(text)
	meta := Event{
		Direction: DirIn,
		Source:    source,
		Bytes:     size,
		Hash:      protocol.HashPrefix(hash),
	}

	snap, err := e.clip.Read()
	if err != nil {
		if errors.Is(err, clipboard.ErrBusy) {
			e.record(meta, StatusBusy, "剪贴板被占用，已放弃本次写入")
			return Result{Status: StatusBusy}, err
		}
		e.record(meta, StatusBusy, "读取剪贴板失败")
		return Result{Status: StatusBusy}, clipboard.ErrBusy
	}

	// 安卓端会退避重试同一条内容：内容相同即视为成功，不重复写剪贴板。
	if snap.Kind == clipboard.KindText && snap.Text == text {
		etag := e.commitCache(snap.Seq, text, false)
		e.record(meta, StatusDuplicate, "内容与当前剪贴板一致，未重复写入")
		return Result{ETag: etag, Status: StatusDuplicate}, nil
	}

	newSeq, err := e.clip.Write(text, snap.Seq)
	if err != nil {
		switch {
		case errors.Is(err, clipboard.ErrConflict):
			e.record(meta, StatusConflict, "写入前剪贴板已被其它程序改变")
			return Result{Status: StatusConflict}, err
		default:
			e.record(meta, StatusBusy, "剪贴板被占用，重试后仍未打开")
			return Result{Status: StatusBusy}, clipboard.ErrBusy
		}
	}

	etag := e.commitCache(newSeq, text, true)
	e.mu.Lock()
	// 手机写进来的内容不继承电脑端某段内容的"手动放行"。
	e.forceHash = ""
	e.mu.Unlock()
	e.record(meta, StatusSynced, "")
	return Result{ETag: etag, Status: StatusSynced}, nil
}

// MarkInvalid 记录一次非法请求（不记录正文）。
func (e *Engine) MarkInvalid(direction, source, note string) {
	e.record(Event{Direction: direction, Source: source}, StatusInvalid, note)
}

// oversizeNote 判断一段文本为什么不能推出，返回空串表示可以推。
//
// 顺序固定：先看协议硬上限（64 KiB，手机端同样按这个数拒绝，强推也突破不了），
// 再看本地配置的字符上限。force 用于区分「手动推送上限」与常规上限的文案。
func (e *Engine) oversizeNote(limit int, text string, force bool) string {
	if len(text) > protocol.MaxTextBytes {
		return fmt.Sprintf("超过协议上限 %d KiB，手机端无法接收", protocol.MaxTextBytes/1024)
	}
	if limit > 0 && utf8.RuneCountInString(text) > limit {
		if force {
			return fmt.Sprintf("超过手动推送上限 %d 字符，未推送", limit)
		}
		return fmt.Sprintf("超过 %d 字符上限，未推送", limit)
	}
	return ""
}

// commitCache 更新缓存；selfWrite 为真时登记自写序列号以抑制回环事件。
func (e *Engine) commitCache(seq uint32, text string, selfWrite bool) string {
	etag := protocol.ETag(text)
	_, hash := protocol.Digest(text)
	e.mu.Lock()
	e.kind = clipboard.KindText
	e.text = text
	e.etag = etag
	e.lastSeq = seq
	e.seenHash, e.seenStatus = hash, StatusSynced
	if selfWrite {
		e.selfSeq = seq
	}
	e.mu.Unlock()
	return etag
}

// clearCache 让缓存失效并推进序列号：手机下次 GET 会拿到 204，保持不变。
func (e *Engine) clearCache(seq uint32) {
	e.mu.Lock()
	e.kind = clipboard.KindEmpty
	e.text, e.etag = "", ""
	e.lastSeq = seq
	// 没有可推送的内容时，上一次手动推送的放行也一并作废。
	e.forceHash = ""
	e.mu.Unlock()
}

// forceCommit 写入手动推送的内容，并在同一把锁里登记放行标记。
//
// 两步必须原子完成：若先写缓存再登记放行，轮询可能恰好插在中间，看到"缓存里有超长
// 内容、却没有放行标记"，于是立刻把它撤下——用户刚点的推送就白做了。
func (e *Engine) forceCommit(seq uint32, text string) string {
	etag := protocol.ETag(text)
	_, hash := protocol.Digest(text)
	e.mu.Lock()
	e.kind = clipboard.KindText
	e.text = text
	e.etag = etag
	e.lastSeq = seq
	e.seenHash, e.seenStatus = hash, StatusSynced
	e.forceHash = hash
	e.mu.Unlock()
	return etag
}

func (e *Engine) watch() {
	defer close(e.done)
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-ticker.C:
			e.poll()
		}
	}
}

// poll 检查序列号，只有变化时才读全文。
func (e *Engine) poll() {
	if !e.enabled.Load() {
		return
	}
	seq := e.clip.Sequence()
	if seq == 0 {
		return
	}
	e.mu.RLock()
	last := e.lastSeq
	self := e.selfSeq
	e.mu.RUnlock()
	if seq == last {
		return
	}

	snap, err := e.clip.Read()
	if err != nil {
		// 读取失败通常是被占用：序列号未推进，下一轮自然重试。
		return
	}
	if snap.Seq == 0 {
		snap.Seq = seq
	}

	if self != 0 && snap.Seq == self {
		// 自写抑制：只推进序列号，不产生事件。
		e.mu.Lock()
		e.selfSeq = 0
		e.lastSeq = snap.Seq
		e.mu.Unlock()
		return
	}

	skip := e.shouldSkip(snap)

	switch {
	case skip:
		// 敏感内容不进缓存：GET 返回 204，安卓端据此清空 ETag 且不动手机剪贴板。
		e.mu.Lock()
		e.kind = clipboard.KindEmpty
		e.text = ""
		e.etag = ""
		e.lastSeq = snap.Seq
		e.seenHash, e.seenStatus = "", ""
		e.mu.Unlock()
		e.record(Event{Direction: DirOut, Source: protocol.SourceDesktop, Bytes: len(snap.Text)},
			StatusSensitive, "已跳过同步："+snap.SensitiveReason)

	case snap.Kind == clipboard.KindText:
		_, hash := protocol.Digest(snap.Text)

		ev := Event{
			Direction: DirOut,
			Source:    protocol.SourceDesktop,
			Bytes:     len(snap.Text),
			Hash:      protocol.HashPrefix(hash),
			Preview:   previewText(snap.Text),
		}
		limit := e.pushLimit()

		e.mu.Lock()
		// 放行状态在写锁内确认：读剪贴板与这里之间可能刚有人点了托盘「推送」。
		// 手动推送过的同一段内容不再按常规字符上限拦截——否则系统或其它程序把剪贴板
		// 原地重设一次（序列号变了、内容没变），刚强推的内容就会被立刻撤下。
		// 协议硬上限（64 KiB）不因强推而放宽：那是手机端的接收上限。
		forced := e.forceHash != "" && e.forceHash == hash
		oversize := len(snap.Text) > protocol.MaxTextBytes ||
			(limit > 0 && utf8.RuneCountInString(snap.Text) > limit && !forced)
		status := StatusSynced
		if oversize {
			// 超长内容不进缓存：GET 返回 204，手机端保持原样，不会被大段文本卡住。
			status = StatusOversize
			e.kind = clipboard.KindEmpty
			e.text, e.etag = "", ""
		} else {
			e.kind = clipboard.KindText
			e.text = snap.Text
			e.etag = protocol.ETag(snap.Text)
		}
		e.lastSeq = snap.Seq
		repeat := e.seenHash == hash && e.seenStatus == status
		e.seenHash, e.seenStatus = hash, status
		if !forced {
			// 内容换了，上一次手动推送的放行随之作废。
			e.forceHash = ""
		}
		e.mu.Unlock()

		// 同样的内容、同样的结论：不再重复记一条（剪贴板序列号会被反复推高）。
		if repeat {
			return
		}
		switch {
		case oversize:
			e.record(ev, StatusOversize, e.oversizeNote(limit, snap.Text, false))
		default:
			e.record(ev, StatusSynced, "")
		}

	default:
		// 空剪贴板 / 图片 / 文件：不产生事件，但缓存必须失效。
		// 去重标记一并清掉：内容离开过剪贴板，再复制回同样的文字算一次新推送。
		e.mu.Lock()
		e.kind = snap.Kind
		e.text = ""
		e.etag = ""
		e.lastSeq = snap.Seq
		e.seenHash, e.seenStatus = "", ""
		e.mu.Unlock()
	}
}

// refresh 主动读取一次剪贴板（启动预热、修改上限后立即重算）。
// 超长内容与敏感内容都会记一条事件，让用户知道"为什么没同步"。
func (e *Engine) refresh(recordSensitive bool) {
	snap, err := e.clip.Read()
	if err != nil {
		return
	}
	seq := snap.Seq
	if seq == 0 {
		seq = e.clip.Sequence()
	}
	limit := e.pushLimit()
	hash := ""
	if snap.Kind == clipboard.KindText {
		_, hash = protocol.Digest(snap.Text)
	}
	e.mu.Lock()
	// 与 poll 一致：放行状态与读到的内容在同一把锁内比对。
	forced := hash != "" && e.forceHash != "" && e.forceHash == hash
	// 手动推送只放宽本地字符上限；协议 64 KiB 硬上限不放宽（手机端同样按它拒绝）。
	bytesOver := snap.Kind == clipboard.KindText && len(snap.Text) > protocol.MaxTextBytes
	charsOver := snap.Kind == clipboard.KindText && limit > 0 && utf8.RuneCountInString(snap.Text) > limit
	oversize := bytesOver || (charsOver && !forced)
	skip := e.shouldSkip(snap)

	status := StatusSynced
	if oversize {
		status = StatusOversize
	}

	e.lastSeq = seq
	repeat := hash != "" && e.seenHash == hash && e.seenStatus == status
	if hash != "" {
		e.seenHash, e.seenStatus = hash, status
	} else if snap.Kind != clipboard.KindText {
		// 非文本/空：内容已离开剪贴板，去重标记作废。
		e.seenHash, e.seenStatus = "", ""
	}
	if snap.Kind == clipboard.KindText && !forced {
		// 内容换了，上一次手动推送的放行随之作废。
		e.forceHash = ""
	}
	switch {
	case skip, oversize:
		e.kind = clipboard.KindEmpty
		e.text, e.etag = "", ""
	case snap.Kind == clipboard.KindText:
		e.kind = clipboard.KindText
		e.text = snap.Text
		e.etag = protocol.ETag(snap.Text)
	default:
		e.kind = snap.Kind
		e.text, e.etag = "", ""
	}
	e.mu.Unlock()

	if skip && recordSensitive {
		e.record(Event{Direction: DirOut, Source: protocol.SourceDesktop, Bytes: len(snap.Text)},
			StatusSensitive, "已跳过同步："+snap.SensitiveReason)
		return
	}
	if oversize && !repeat {
		e.record(Event{Direction: DirOut, Source: protocol.SourceDesktop, Bytes: len(snap.Text),
			Preview: previewText(snap.Text)}, StatusOversize, e.oversizeNote(limit, snap.Text, false))
	}
}

// record 补齐预览并写入事件环。
func (e *Engine) record(ev Event, status, note string) {
	ev.Status = status
	ev.Note = note
	if ev.Direction == "" {
		ev.Direction = DirOut
	}
	if ev.Source == "" {
		if ev.Direction == DirIn {
			ev.Source = protocol.SourceAndroid
		} else {
			ev.Source = protocol.SourceDesktop
		}
	}
	// 预览始终只在内存里：手机推来的内容成功后补一份，方便对照。
	if ev.Preview == "" && (status == StatusSynced || status == StatusDuplicate) && ev.Direction == DirIn {
		if text, _, kind := e.Current(); kind == clipboard.KindText && text != "" {
			ev.Preview = previewText(text)
		}
	}
	e.ring.Add(ev)
}

// previewText 把正文压成单行，超长时保留头尾、中间省略：
// 事件栏只有一行高度，用户需要看到开头判断是什么，也需要看到结尾判断是否完整。
func previewText(s string) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= PreviewLimit {
		return string(r)
	}
	return string(r[:previewHead]) + "…" + string(r[len(r)-previewTail:])
}
