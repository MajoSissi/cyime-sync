package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"cyime-sync/internal/config"
	"cyime-sync/internal/winutil"
)

// settingsView 是设置页可见的字段。
type settingsView struct {
	Enabled          bool   `json:"enabled"`
	ListenAddress    string `json:"listen_address"`
	Port             int    `json:"port"`
	MaxPushChars     int    `json:"max_push_chars"`
	ForcePushChars   int    `json:"force_push_chars"`
	SkipSuspectedOTP bool   `json:"skip_suspected_otp"`
	AutoStart        bool   `json:"auto_start"`
}

type settingsResponse struct {
	Settings  settingsView `json:"settings"`
	Listen    string       `json:"listen"`
	Addresses []string     `json:"addresses"`
	LastError string       `json:"last_error,omitempty"`
}

// settingsPatch 全是可选字段，支持局部更新（前端每次只发改动的那一项）。
type settingsPatch struct {
	Enabled          *bool   `json:"enabled"`
	ListenAddress    *string `json:"listen_address"`
	Port             *int    `json:"port"`
	MaxPushChars     *int    `json:"max_push_chars"`
	ForcePushChars   *int    `json:"force_push_chars"`
	SkipSuspectedOTP *bool   `json:"skip_suspected_otp"`
	AutoStart        *bool   `json:"auto_start"`
}

func (m *Manager) view() settingsResponse {
	c := m.opts.Config.Get()
	return settingsResponse{
		Settings: settingsView{
			Enabled:          c.Enabled,
			ListenAddress:    c.ListenAddress,
			Port:             c.Port,
			MaxPushChars:     c.MaxPushChars,
			ForcePushChars:   c.ForcePushChars,
			SkipSuspectedOTP: c.SkipSuspectedOTP,
			AutoStart:        c.AutoStart,
		},
		Listen:    c.Listen(),
		Addresses: winutil.LocalIPv4s(),
		LastError: m.LastError(),
	}
}

// handleSettings 处理 GET（读取设置）与 PUT（局部更新）。
func (m *Manager) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if !m.localAuthed(r) {
			writeError(w, http.StatusForbidden, "local_only", "设置接口仅限本机访问")
			return
		}
		writeJSON(w, http.StatusOK, m.view())

	case http.MethodPut:
		if !m.localAuthed(r) {
			writeError(w, http.StatusForbidden, "local_only", "设置接口仅限本机访问")
			return
		}
		if !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "bad_origin", "拒绝跨站写入")
			return
		}
		m.updateSettings(w, r)

	default:
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, http.StatusMethodNotAllowed, "method", "只支持 GET / PUT")
	}
}

func (m *Manager) updateSettings(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_body", "读取请求体失败")
		return
	}
	var patch settingsPatch
	if err := json.Unmarshal(body, &patch); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "请求体不是合法 JSON")
		return
	}

	prev := m.opts.Config.Get()
	listenChanged := false

	// 自启动不是纯配置项：先写注册表，失败则不落盘，避免界面与实际不一致。
	if patch.AutoStart != nil && *patch.AutoStart != prev.AutoStart {
		if err := m.opts.ApplyAutoStart(*patch.AutoStart); err != nil {
			writeError(w, http.StatusBadRequest, "autostart_failed", err.Error())
			return
		}
	}

	next, err := m.opts.Config.Update(func(c *config.Config) error {
		if patch.Enabled != nil {
			c.Enabled = *patch.Enabled
			listenChanged = listenChanged || (prev.Enabled != c.Enabled)
		}
		if patch.ListenAddress != nil {
			if err := config.ValidateListenAddress(*patch.ListenAddress); err != nil {
				return err
			}
			if c.ListenAddress != *patch.ListenAddress {
				listenChanged = true
			}
			c.ListenAddress = *patch.ListenAddress
		}
		if patch.Port != nil {
			if *patch.Port < 1 || *patch.Port > 65535 {
				return &FieldError{Field: "port", Message: "端口必须在 1-65535 之间"}
			}
			if c.Port != *patch.Port {
				listenChanged = true
			}
			c.Port = *patch.Port
		}
		if patch.MaxPushChars != nil {
			if *patch.MaxPushChars < config.MinPushChars || *patch.MaxPushChars > config.MaxPushChars {
				return &FieldError{Field: "max_push_chars", Message: fmt.Sprintf("推送字符上限需在 %d-%d 之间", config.MinPushChars, config.MaxPushChars)}
			}
			c.MaxPushChars = *patch.MaxPushChars
		}
		if patch.ForcePushChars != nil {
			if *patch.ForcePushChars < config.MinPushChars || *patch.ForcePushChars > config.MaxPushChars {
				return &FieldError{Field: "force_push_chars", Message: fmt.Sprintf("手动推送字符上限需在 %d-%d 之间", config.MinPushChars, config.MaxPushChars)}
			}
			c.ForcePushChars = *patch.ForcePushChars
		}
		if patch.SkipSuspectedOTP != nil {
			c.SkipSuspectedOTP = *patch.SkipSuspectedOTP
		}
		if patch.AutoStart != nil {
			c.AutoStart = *patch.AutoStart
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	// 预览恒开、方向恒开，配置只影响"推到手机"的长度上限与敏感猜测策略。
	m.opts.Engine.SetEnabled(next.Enabled)
	if patch.MaxPushChars != nil || patch.SkipSuspectedOTP != nil {
		// 上限或敏感策略变化要立刻对当前剪贴板重算：改小/收紧要马上撤下缓存，
		// 改大/放宽要马上让当前内容可推送，而不是等用户再复制一次。
		m.opts.Engine.Refresh()
	}

	resp := m.view()
	// 先把响应写回，再异步重建监听，避免设置页拿到连接被重置的错误。
	writeJSON(w, http.StatusOK, resp)

	if listenChanged {
		m.scheduleApply()
	}
}

// FieldError 表示表单字段级错误。
type FieldError struct {
	Field   string
	Message string
}

func (e *FieldError) Error() string { return e.Message }
