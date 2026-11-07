package server

import (
	"errors"
	"io"
	"net/http"

	"cyime-sync/internal/clipboard"
	"cyime-sync/internal/protocol"
	"cyime-sync/internal/syncer"
)

// handleClipboard 是协议入口：GET 读取、PUT 写入。认证只接受设备令牌（Bearer）。
func (m *Manager) handleClipboard(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		m.clipboardGet(w, r)
	case http.MethodPut, http.MethodPost:
		m.clipboardPut(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, protocol.StatusBadRequest, protocol.CodeBadRequest, "只支持 GET / PUT")
	}
}

// clipboardGet 返回电脑当前剪贴板文本。
//
// 手机端每约 2 秒调用一次，因此必须廉价且幂等：
// 只读引擎缓存，不打开剪贴板、不刷新 ETag、不记录正文。
func (m *Manager) clipboardGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := m.device(r); !ok {
		writeUnauthorized(w)
		return
	}
	cfg := m.opts.Config.Get()
	if !cfg.Enabled {
		writeError(w, protocol.StatusUnavailable, protocol.CodeUnavailable, "电脑端同步已停用")
		return
	}

	text, etag, kind := m.opts.Engine.Current()
	if kind != clipboard.KindText || text == "" {
		// 空剪贴板或非文本：204，手机端据此清空本地 ETag 且不动手机剪贴板。
		w.WriteHeader(protocol.StatusNoContent)
		return
	}
	if etag == "" {
		etag = protocol.ETag(text)
	}
	if matchETag(r.Header.Get("If-None-Match"), etag) {
		w.Header().Set("ETag", etag)
		w.WriteHeader(protocol.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, protocol.StatusOK, protocol.BuildProfile(text, protocol.SourceDesktop))
}

// clipboardPut 接收手机端推送的文本并写入电脑剪贴板。
//
// 顺序固定为「停用 -> 方向开关 -> 请求体 -> 写入」，与手机端 responseError
// 的语义一一对应。
func (m *Manager) clipboardPut(w http.ResponseWriter, r *http.Request) {
	dev, ok := m.device(r)
	if !ok {
		writeUnauthorized(w)
		return
	}
	cfg := m.opts.Config.Get()
	if !cfg.Enabled {
		writeError(w, protocol.StatusUnavailable, protocol.CodeUnavailable, "电脑端同步已停用")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, protocol.MaxBodyBytes+1))
	if err != nil {
		writeError(w, protocol.StatusBadRequest, protocol.CodeBadRequest, "读取请求体失败")
		return
	}
	if len(body) > protocol.MaxBodyBytes {
		writeError(w, protocol.StatusTooLarge, protocol.CodeTooLarge, "请求体超过 256 KiB")
		return
	}
	source := protocol.RequestSource(body)

	text, perr := protocol.ParseRequest(body)
	if perr != nil {
		m.opts.Engine.MarkInvalid(syncer.DirIn, source, perr.Error())
		writeProtocolError(w, perr)
		return
	}

	// 先登记来源再写缓存：推送器每 300ms 检查一次缓存，若在「写完缓存」与「登记来源」
	// 之间插入，就会把这条内容原样推回给刚发来的那台手机。
	if m.opts.Relay != nil {
		_, hash := protocol.Digest(text)
		m.opts.Relay.NoteInbound(dev.ID, hash)
	}

	if _, applyErr := m.opts.Engine.Apply(text, sourceOr(source, dev.Name)); applyErr != nil {
		if errors.Is(applyErr, clipboard.ErrConflict) {
			writeError(w, protocol.StatusConflict, protocol.CodeConflict, "写入前剪贴板已被其它程序改变")
			return
		}
		writeError(w, protocol.StatusUnavailable, protocol.CodeUnavailable, "剪贴板被占用，请稍后重试")
		return
	}

	// 200 响应必须回显与请求相同的 hash，否则手机端视为未确认。
	w.Header().Set("ETag", protocol.ETag(text))
	writeJSON(w, protocol.StatusOK, protocol.BuildProfile(text, protocol.SourceDesktop))
}

func sourceOr(src, fallback string) string {
	if src == "" {
		return fallback
	}
	return src
}
