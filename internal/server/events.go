package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const heartbeatInterval = 20 * time.Second

// handleEvents 提供 SSE 事件流（GET）与清空事件（DELETE）。
//
// 只对本机 + 令牌开放：事件里可能含正文预览，不能给局域网其它主机读。
func (m *Manager) handleEvents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodDelete:
		if !m.localAuthed(r) || !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "local_only", "设置接口仅限本机访问")
			return
		}
		m.opts.Engine.Ring().Clear()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return

	case http.MethodGet:
		if !m.localAuthed(r) {
			writeError(w, http.StatusForbidden, "local_only", "事件流仅限本机访问")
			return
		}
		m.streamEvents(w, r)
		return

	default:
		w.Header().Set("Allow", "GET, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method", "只支持 GET / DELETE")
	}
}

func (m *Manager) streamEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_stream", "当前连接不支持流式输出")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// 先订阅再发首帧，避免这两步之间产生的事件被漏掉。
	ch, cancel := m.opts.Engine.Ring().Subscribe()
	defer cancel()

	pairCh, unsubscribe := m.subscribePairing()
	defer unsubscribe()

	// 首帧只给状态与配对状态，不再下发历史事件：设置页只展示「本次打开之后」的同步行为，
	// 刷新即清空。历史仍在服务端环里（供日志/排障），但页面既不需要也不该一次画几百条。
	writeSSE(w, "snapshot", map[string]any{
		"status":  m.statusView(),
		"pairing": m.pairingState(),
	})
	flusher.Flush()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			writeSSE(w, "sync", ev)
			flusher.Flush()
		case msg, ok := <-pairCh:
			if !ok {
				return
			}
			kind, detail, _ := strings.Cut(msg, "\x00")
			writeSSE(w, "pairing", map[string]any{
				"event":  kind,
				"detail": detail,
				"state":  m.pairingState(),
			})
			flusher.Flush()
		case <-ticker.C:
			// 注释行心跳，避免中间设备因空闲而断链。
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSE 输出一条 SSE 消息。
func writeSSE(w io.Writer, event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}
