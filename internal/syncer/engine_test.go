package syncer

import (
	"errors"
	"strings"
	"testing"

	"cyime-sync/internal/clipboard"
	"cyime-sync/internal/protocol"
)

func newEngine(t *testing.T, initial string) (*Engine, *clipboard.Memory, *Ring) {
	t.Helper()
	clip := clipboard.NewMemory(initial)
	ring := NewRing(20)
	e := New(clip, ring)
	e.refresh(false)
	return e, clip, ring
}

func TestApplyWritesClipboardAndSuppressesLoopback(t *testing.T) {
	e, clip, ring := newEngine(t, "初始")

	res, err := e.Apply("来自手机", protocol.SourceAndroid)
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if res.Status != StatusSynced || res.ETag != protocol.ETag("来自手机") {
		t.Fatalf("结果不符: %+v", res)
	}

	// 自写抑制：轮询不得把本次写入当成本机新内容。
	e.poll()

	var outs, ins int
	for _, ev := range ring.List() {
		switch ev.Direction {
		case DirOut:
			outs++
		case DirIn:
			ins++
		}
	}
	if outs != 0 {
		t.Fatalf("自写产生了 %d 条外发事件，应被抑制", outs)
	}
	if ins != 1 {
		t.Fatalf("应记录 1 条入向事件，实际 %d", ins)
	}

	snap, err := clip.Read()
	if err != nil || snap.Text != "来自手机" {
		t.Fatalf("剪贴板未写入: %v %q", err, snap.Text)
	}
}

func TestApplyDuplicateDoesNotRewrite(t *testing.T) {
	e, clip, _ := newEngine(t, "同样的内容")
	before := clip.Sequence()

	res, err := e.Apply("同样的内容", protocol.SourceAndroid)
	if err != nil {
		t.Fatalf("重复内容应视为成功: %v", err)
	}
	if res.Status != StatusDuplicate {
		t.Fatalf("状态应为 duplicate，实际 %s", res.Status)
	}
	if clip.Sequence() != before {
		t.Fatal("重复内容不应再次写剪贴板（序列号被推进）")
	}
}

type conflictClip struct{ *clipboard.Memory }

func (c *conflictClip) Write(string, uint32) (uint32, error) { return 0, clipboard.ErrConflict }

func TestApplyConflictIsReported(t *testing.T) {
	mem := clipboard.NewMemory("旧内容")
	ring := NewRing(10)
	e := New(&conflictClip{Memory: mem}, ring)
	e.refresh(false)

	_, err := e.Apply("新内容", protocol.SourceAndroid)
	if !errors.Is(err, clipboard.ErrConflict) {
		t.Fatalf("应返回 ErrConflict，实际 %v", err)
	}
	last, ok := ring.Last()
	if !ok || last.Status != StatusConflict {
		t.Fatalf("应记录冲突事件，实际 %+v", last)
	}
}

func TestPollDetectsExternalChange(t *testing.T) {
	e, clip, ring := newEngine(t, "旧")
	if _, err := clip.Write("新内容 by 用户", 0); err != nil {
		t.Fatal(err)
	}

	e.poll()

	text, etag, kind := e.Current()
	if kind != clipboard.KindText || text != "新内容 by 用户" {
		t.Fatalf("缓存未更新: %q", text)
	}
	if etag != protocol.ETag("新内容 by 用户") {
		t.Fatalf("ETag 不符: %s", etag)
	}
	last, _ := ring.Last()
	if last.Direction != DirOut || last.Status != StatusSynced {
		t.Fatalf("应记录外发事件，实际 %+v", last)
	}
}

func TestSensitiveContentIsNotCached(t *testing.T) {
	e, clip, ring := newEngine(t, "普通内容")
	// 来源自己声明的隐私标记不受用户开关约束：即使关掉"跳过疑似验证码"也必须跳过。
	e.SetSkipGuessFunc(func() bool { return false })
	if _, err := clip.Write("123456", 0); err != nil {
		t.Fatal(err)
	}
	clip.SetSensitive("内容来源禁止上传云端（CanUploadToCloudClipboard=0）")

	e.poll()

	text, etag, kind := e.Current()
	if kind == clipboard.KindText || text != "" || etag != "" {
		t.Fatalf("敏感内容不得进入缓存: kind=%v text=%q", kind, text)
	}
	last, ok := ring.Last()
	if !ok || last.Status != StatusSensitive {
		t.Fatalf("应记录敏感跳过事件，实际 %+v", last)
	}
	if last.Preview != "" {
		t.Fatal("敏感事件不得带正文预览")
	}
}

// 疑似验证码只是形态猜测（也可能是订单号、工号），开关关闭后必须放行。
func TestSuspectedOTPRespectsSwitch(t *testing.T) {
	e, clip, ring := newEngine(t, "普通内容")
	if _, err := clip.Write("863921", 0); err != nil {
		t.Fatal(err)
	}
	clip.SetGuessSensitive("疑似短信验证码（纯数字短文本）")

	// 默认开启：跳过并标注原因。
	e.poll()
	if _, _, kind := e.Current(); kind == clipboard.KindText {
		t.Fatal("开关开启时验证码不应进入缓存")
	}
	if last, _ := ring.Last(); last.Status != StatusSensitive {
		t.Fatalf("应记录敏感跳过事件，实际 %+v", last)
	}

	// 关闭开关：同一条内容立即可推送，不必等用户重新复制。
	e.SetSkipGuessFunc(func() bool { return false })
	e.refresh(false)
	text, _, kind := e.Current()
	if kind != clipboard.KindText || text != "863921" {
		t.Fatalf("关闭开关后应正常缓存: kind=%v text=%q", kind, text)
	}

	// 再复制一次同样内容：事件按普通推送记录，带预览。
	clip.Clear()
	e.poll()
	if _, err := clip.Write("863921", 0); err != nil {
		t.Fatal(err)
	}
	clip.SetGuessSensitive("疑似短信验证码（纯数字短文本）")
	e.poll()

	last, _ := ring.Last()
	if last.Status != StatusSynced || last.Preview != "863921" {
		t.Fatalf("关闭开关后应记录正常推送，实际 %+v", last)
	}
}

// 预览恒开：成功事件都带正文，超长时保留头尾并压成单行。
func TestPreviewAlwaysOnAndSingleLine(t *testing.T) {
	e, _, ring := newEngine(t, "")

	if _, err := e.Apply("预览内容", protocol.SourceAndroid); err != nil {
		t.Fatal(err)
	}
	last, _ := ring.Last()
	if last.Preview != "预览内容" {
		t.Fatalf("事件应带正文预览，实际 %q", last.Preview)
	}

	long := strings.Repeat("头", 200) + "\r\n\t" + strings.Repeat("尾", 200)
	if _, err := e.Apply(long, protocol.SourceAndroid); err != nil {
		t.Fatal(err)
	}
	last, _ = ring.Last()
	if strings.ContainsAny(last.Preview, "\r\n\t") {
		t.Fatalf("预览必须压成单行，实际 %q", last.Preview)
	}
	if !strings.HasPrefix(last.Preview, "头头头") || !strings.HasSuffix(last.Preview, "尾尾尾") {
		t.Fatalf("预览应保留头尾，实际 %q", last.Preview)
	}
	if !strings.Contains(last.Preview, "…") {
		t.Fatalf("超长预览应有省略号，实际 %q", last.Preview)
	}
	if n := len([]rune(last.Preview)); n > PreviewLimit+1 {
		t.Fatalf("预览过长: %d 字符", n)
	}
}

// 超过推送上限：不进缓存（手机拿 204），并记录超长事件。
func TestOversizeTextIsNotCached(t *testing.T) {
	e, clip, ring := newEngine(t, "初始")
	e.SetPushLimitFunc(func() int { return 5 })

	if _, err := clip.Write("1234567", clip.Sequence()); err != nil {
		t.Fatal(err)
	}
	e.poll()

	if text, etag, kind := e.Current(); kind == clipboard.KindText || text != "" || etag != "" {
		t.Fatalf("超长内容不得进入缓存: kind=%v text=%q", kind, text)
	}
	last, ok := ring.Last()
	if !ok || last.Status != StatusOversize {
		t.Fatalf("应记录超长跳过事件，实际 %+v", last)
	}

	// 刚好等于上限仍要推送。
	if _, err := clip.Write("12345", clip.Sequence()); err != nil {
		t.Fatal(err)
	}
	e.poll()
	if text, _, _ := e.Current(); text != "12345" {
		t.Fatalf("等于上限应正常缓存，实际 %q", text)
	}
}

func TestRingCapacityAndClear(t *testing.T) {
	ring := NewRing(3)
	for i := 0; i < 10; i++ {
		ring.Add(Event{Direction: DirIn, Status: StatusSynced})
	}
	if ring.Len() != 3 {
		t.Fatalf("环形缓冲应只保留 3 条，实际 %d", ring.Len())
	}
	list := ring.List()
	if list[0].ID <= list[len(list)-1].ID {
		t.Fatal("列表应最新在前")
	}
	ring.Clear()
	if ring.Len() != 0 {
		t.Fatal("清空失败")
	}
}

func TestEmptyClipboardProducesNoEvent(t *testing.T) {
	e, clip, ring := newEngine(t, "内容")
	clip.SetNonText()
	e.poll()

	_, _, kind := e.Current()
	if kind != clipboard.KindNonText {
		t.Fatalf("非文本应清空文本缓存，实际 kind=%v", kind)
	}
	for _, ev := range ring.List() {
		if ev.Direction == DirOut {
			t.Fatalf("图片/文件不应产生同步事件: %+v", ev)
		}
	}
}

// 强制推送只放宽长度限制：常规轮询会跳过超长内容，托盘手动推送却能送出去。
func TestForcePushBypassesNormalLimit(t *testing.T) {
	long := strings.Repeat("中", 1500)
	e, _, ring := newEngine(t, long)
	e.SetPushLimitFunc(func() int { return 1000 })
	e.SetForceLimitFunc(func() int { return 10000 })

	// 触发一次常规重算：超长内容不得进入缓存。
	e.Refresh()
	if _, _, kind := e.Current(); kind == clipboard.KindText {
		t.Fatal("超过普通上限的内容不应进入缓存")
	}
	last, _ := ring.Last()
	if last.Status != StatusOversize {
		t.Fatalf("应先记录超长事件，实际 %+v", last)
	}

	res, err := e.ForcePush()
	if err != nil {
		t.Fatalf("强制推送失败: %v", err)
	}
	if res.Status != StatusSynced {
		t.Fatalf("强制推送应成功，实际 %s", res.Status)
	}
	text, etag, kind := e.Current()
	if kind != clipboard.KindText || text != long || etag != protocol.ETag(long) {
		t.Fatalf("强制推送后缓存不符: kind=%v len=%d", kind, len(text))
	}
	last, _ = ring.Last()
	if last.Status != StatusSynced || last.Note != "已手动推送" {
		t.Fatalf("应记录强制推送事件，实际 %+v", last)
	}
}

// 手动推送之后，同一段内容被别的程序「原地重设」（序列号变了、内容没变）时，
// 不能又被常规上限撤下：否则手机下一次 GET 拿到 204，用户看到的就是「强推了却没同步」。
func TestForcePushStaysAvailableAfterSequenceBump(t *testing.T) {
	long := strings.Repeat("中", 1500)
	e, clip, ring := newEngine(t, long)
	e.SetPushLimitFunc(func() int { return 1000 })
	e.SetForceLimitFunc(func() int { return 10000 })

	e.Refresh() // 先按常规上限撤下缓存
	if _, _, kind := e.Current(); kind == clipboard.KindText {
		t.Fatal("前置条件：超长内容初始不应在缓存里")
	}
	if _, err := e.ForcePush(); err != nil {
		t.Fatalf("强制推送失败: %v", err)
	}

	if _, err := clip.Write(long, 0); err != nil { // 内容相同，序列号推进
		t.Fatal(err)
	}
	before := len(ring.List())
	e.poll()

	text, _, kind := e.Current()
	if kind != clipboard.KindText || text != long {
		t.Fatalf("同一段内容再次被观测后不应撤下缓存: kind=%v len=%d", kind, len(text))
	}
	if after := len(ring.List()); after != before {
		t.Fatalf("内容与结论都没变，不应重复记事件（新增 %d 条）", after-before)
	}
}

// 剪贴板换成另一段超长内容后，之前的手动放行必须失效。
func TestForcePushReleasesWhenClipboardChanges(t *testing.T) {
	long := strings.Repeat("中", 1500)
	other := strings.Repeat("文", 1500)
	e, clip, ring := newEngine(t, long)
	e.SetPushLimitFunc(func() int { return 1000 })
	e.SetForceLimitFunc(func() int { return 10000 })

	if _, err := e.ForcePush(); err != nil {
		t.Fatalf("强制推送失败: %v", err)
	}
	if _, err := clip.Write(other, 0); err != nil {
		t.Fatal(err)
	}
	e.poll()

	if _, _, kind := e.Current(); kind == clipboard.KindText {
		t.Fatal("换成新内容后应按常规上限判定，不得沿用上一次的放行")
	}
	if last, _ := ring.Last(); last.Status != StatusOversize {
		t.Fatalf("应记录超长事件，实际 %+v", last)
	}
}

// 同一段超长内容反复被观测（剪贴板序列号被推高）只记一条事件，不刷屏。
func TestIdenticalOversizeRecordedOnce(t *testing.T) {
	long := strings.Repeat("中", 1500)
	e, clip, ring := newEngine(t, long)
	e.SetPushLimitFunc(func() int { return 1000 })

	e.Refresh()
	if last, _ := ring.Last(); last.Status != StatusOversize {
		t.Fatalf("首次应记录超长事件，实际 %+v", last)
	}
	before := len(ring.List())

	for i := 0; i < 3; i++ {
		if _, err := clip.Write(long, 0); err != nil {
			t.Fatal(err)
		}
		e.poll()
	}
	if after := len(ring.List()); after != before {
		t.Fatalf("内容未变不应重复记事件（新增 %d 条）", after-before)
	}
}

// 协议硬上限（64 KiB）连手动推送也突破不了：手机端按这个数拒绝，报"已推送"是假成功。
func TestForcePushRespectsProtocolMaxBytes(t *testing.T) {
	huge := strings.Repeat("中", 22000) // 66000 字节 > 64 KiB
	e, _, ring := newEngine(t, huge)
	e.SetPushLimitFunc(func() int { return 1000 })
	e.SetForceLimitFunc(func() int { return 100000 })

	res, err := e.ForcePush()
	if err != nil {
		t.Fatalf("强制推送不应返回错误: %v", err)
	}
	if res.Status != StatusOversize {
		t.Fatalf("超过协议上限应记为 oversize，实际 %s", res.Status)
	}
	if _, _, kind := e.Current(); kind == clipboard.KindText {
		t.Fatal("超过协议上限的内容不得进入缓存")
	}
	last, _ := ring.Last()
	if last.Status != StatusOversize || !strings.Contains(last.Note, "协议上限") {
		t.Fatalf("事件应说明是协议上限，实际 %+v", last)
	}
}

// 强制推送也不能突破它自己的上限：否则一次误操作就可能发出去几 MB。
func TestForcePushRespectsForceLimit(t *testing.T) {
	e, _, ring := newEngine(t, strings.Repeat("A", 300))
	e.SetForceLimitFunc(func() int { return 100 })

	res, err := e.ForcePush()
	if err != nil {
		t.Fatalf("强制推送不应返回错误: %v", err)
	}
	if res.Status != StatusOversize {
		t.Fatalf("超过强制上限应记为 oversize，实际 %s", res.Status)
	}
	if _, _, kind := e.Current(); kind == clipboard.KindText {
		t.Fatal("超过强制上限的内容不应进入缓存")
	}
	last, _ := ring.Last()
	if last.Status != StatusOversize {
		t.Fatalf("事件状态不符: %+v", last)
	}
}

// 强制推送放宽的是长度，不是隐私策略：来源声明的敏感内容仍然跳过。
func TestForcePushSkipsSensitive(t *testing.T) {
	e, clip, ring := newEngine(t, "密码")
	clip.SetSensitive("密码管理器标记为私密")
	e.SetForceLimitFunc(func() int { return 10000 })

	res, err := e.ForcePush()
	if err != nil {
		t.Fatalf("强制推送不应返回错误: %v", err)
	}
	if res.Status != StatusSensitive {
		t.Fatalf("敏感内容应跳过，实际 %s", res.Status)
	}
	if _, _, kind := e.Current(); kind == clipboard.KindText {
		t.Fatal("敏感内容不得进入缓存")
	}
	last, _ := ring.Last()
	if last.Status != StatusSensitive || last.Preview != "" {
		t.Fatalf("敏感事件不得带正文预览: %+v", last)
	}
}

func TestForcePushWhenDisabled(t *testing.T) {
	e, _, _ := newEngine(t, "内容")
	e.SetEnabled(false)

	if _, err := e.ForcePush(); !errors.Is(err, ErrDisabled) {
		t.Fatalf("停用时应返回 ErrDisabled，实际 %v", err)
	}
}

// 非文本剪贴板没有可推送的内容，但也要给用户一条明确的事件说明。
func TestForcePushOnNonText(t *testing.T) {
	e, clip, ring := newEngine(t, "内容")
	clip.SetNonText()

	res, err := e.ForcePush()
	if err != nil {
		t.Fatalf("非文本不应返回错误: %v", err)
	}
	if res.Status != StatusEmpty {
		t.Fatalf("非文本应记为 empty，实际 %s", res.Status)
	}
	last, _ := ring.Last()
	if last.Status != StatusEmpty || last.Direction != DirOut {
		t.Fatalf("事件不符: %+v", last)
	}
}
