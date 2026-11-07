package server

import (
	"net/http"
	"time"

	"cyime-sync/internal/winutil"
)

// statusView 是只读状态，不含任何秘密（令牌、正文）。
type statusView struct {
	Version       string     `json:"version"`
	UptimeSeconds int64      `json:"uptime_seconds"`
	Enabled       bool       `json:"enabled"`
	Listening     bool       `json:"listening"`
	Listen        string     `json:"listen"`
	Addresses     []string   `json:"addresses"`
	Events        int        `json:"events"`
	LastEventAt   *time.Time `json:"last_event_at,omitempty"`
	LastStatus    string     `json:"last_status,omitempty"`
	Sequence      uint32     `json:"sequence"`
	LastError     string     `json:"last_error,omitempty"`
}

func (m *Manager) statusView() statusView {
	cfg := m.opts.Config.Get()
	ring := m.opts.Engine.Ring()

	v := statusView{
		Version:       m.opts.Version,
		UptimeSeconds: int64(time.Since(m.started).Seconds()),
		Enabled:       cfg.Enabled,
		Listening:     m.Addr() != "",
		Listen:        cfg.Listen(),
		Addresses:     winutil.LocalIPv4s(),
		Events:        ring.Len(),
		Sequence:      m.opts.Engine.SequenceInfo(),
		LastError:     m.LastError(),
	}
	if last, ok := ring.Last(); ok {
		t := last.Time
		v.LastEventAt = &t
		v.LastStatus = last.Status
	}
	return v
}

// handleStatus 是只读接口。
//
// 带 Bearer 令牌时按协议返回连接状态（手机端据此刷新"已连接/方向"文案）；
// 不带令牌时返回本地设置页用的运行状态，二者都不含秘密。
func (m *Manager) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "method", "只支持 GET")
		return
	}
	if tok, isBearer := bearerToken(r); isBearer {
		dev, ok := m.opts.Pairing.Authenticate(tok)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "设备令牌无效或已被撤销")
			return
		}
		// 方向开关已取消（恒为双向），字段保留只为与手机端协议保持兼容。
		//
		// push / push_pin / push_port / push_failed 是 2026-11 手机端新增的一组：
		// 手机看到 push 为真就会 POST /api/peer 登记地址，之后不再每 2 秒轮询电脑，
		// 改由电脑在剪贴板变化时推过去（键盘收起也能收到）。push_failed 为真时手机
		// 退回旧版轮询，所以推送连续失败必须如实上报，不能让手机一直等。
		view := map[string]any{
			"v":               2,
			"connected":       true,
			"send_enabled":    true,
			"receive_enabled": true,
			"device_id":       dev.ID,
			"server_name":     m.opts.Pairing.ServerName(),
			"push":            true,
			"push_pin":        nil,
			"push_port":       nil,
			"push_failed":     m.opts.Pairing.PushFailed(dev.ID),
		}
		if target, ok := m.opts.Pairing.PushTarget(dev.ID); ok {
			view["push_pin"] = target.Fingerprint
			view["push_port"] = target.Port
		}
		writeJSON(w, http.StatusOK, view)
		return
	}
	writeJSON(w, http.StatusOK, m.statusView())
}
