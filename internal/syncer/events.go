package syncer

import (
	"sort"
	"sync"
	"time"
)

// 事件方向：以"电脑端"为参照。
const (
	// DirOut 电脑 -> 手机（手机 GET 读到电脑剪贴板内容）。
	DirOut = "out"
	// DirIn 手机 -> 电脑（手机 PUT 写入电脑剪贴板）。
	DirIn = "in"
)

// 事件状态，供设置页展示与排障。
const (
	// StatusSynced 正常同步完成。
	StatusSynced = "synced"
	// StatusDuplicate 内容与当前剪贴板一致，按成功处理但不重复写入。
	StatusDuplicate = "duplicate"
	// StatusSensitive 命中敏感标记，按策略跳过同步。
	StatusSensitive = "sensitive"
	// StatusOversize 文本超过推送字符上限，未推到手机。
	StatusOversize = "oversize"
	// StatusConflict 写入时剪贴板被其它进程改动（CAS 失败）。
	StatusConflict = "conflict"
	// StatusBusy 剪贴板被占用，重试耗尽。
	StatusBusy = "busy"
	// StatusInvalid 请求内容不合法。
	StatusInvalid = "invalid"
	// StatusEmpty 读取时剪贴板为空或非文本。
	StatusEmpty = "empty"
)

// Event 是事件流中的一条记录，只含元数据（正文预览不落盘）。
type Event struct {
	ID        uint64    `json:"id"`
	Time      time.Time `json:"time"`
	Direction string    `json:"direction"`
	Source    string    `json:"source"`
	Bytes     int       `json:"bytes"`
	Hash      string    `json:"hash,omitempty"`
	Status    string    `json:"status"`
	Note      string    `json:"note,omitempty"`
	Preview   string    `json:"preview,omitempty"`
}

// Ring 是有容量上限的事件环形缓冲，进程重启即丢弃历史。
type Ring struct {
	mu   sync.Mutex
	max  int
	seq  uint64
	list []Event
	subs map[int]chan Event
	next int
}

// NewRing 创建容量为 max 的事件环。
func NewRing(max int) *Ring {
	if max <= 0 {
		max = 200
	}
	return &Ring{max: max, subs: make(map[int]chan Event)}
}

// Add 追加事件并广播给订阅者，返回补齐 ID/时间后的事件。
func (r *Ring) Add(ev Event) Event {
	r.mu.Lock()
	r.seq++
	ev.ID = r.seq
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	r.list = append(r.list, ev)
	if len(r.list) > r.max {
		r.list = r.list[len(r.list)-r.max:]
	}
	subs := make([]chan Event, 0, len(r.subs))
	for _, ch := range r.subs {
		subs = append(subs, ch)
	}
	r.mu.Unlock()

	// 非阻塞投递：慢客户端只丢事件，绝不拖住同步引擎。
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
	return ev
}

// List 返回副本，按时间倒序（最新在前），便于设置页直接渲染。
func (r *Ring) List() []Event {
	r.mu.Lock()
	out := make([]Event, len(r.list))
	copy(out, r.list)
	r.mu.Unlock()

	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// Len 返回当前事件数量。
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.list)
}

// Last 返回最新一条事件。
func (r *Ring) Last() (Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.list) == 0 {
		return Event{}, false
	}
	return r.list[len(r.list)-1], true
}

// Clear 清空事件（ID 保持单调，前端可继续去重）。
func (r *Ring) Clear() {
	r.mu.Lock()
	r.list = nil
	r.mu.Unlock()
}

// Subscribe 注册订阅者，返回事件通道与退订函数。通道带缓冲，满则丢事件。
func (r *Ring) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	r.mu.Lock()
	id := r.next
	r.next++
	r.subs[id] = ch
	r.mu.Unlock()

	// 退订只摘除引用、不关闭通道：Add 在锁外投递，关闭会造成 panic。
	return ch, func() {
		r.mu.Lock()
		delete(r.subs, id)
		r.mu.Unlock()
	}
}
