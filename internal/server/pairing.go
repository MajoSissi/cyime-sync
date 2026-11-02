package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"cyime-sync/internal/pairing"
)

// deviceView 是已配对设备的对外视图（不含令牌哈希）。
type deviceView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
}

// pairingView 是设置页轮询的配对状态。
type pairingView struct {
	Open        bool         `json:"open"`
	SecondsLeft int          `json:"seconds_left"`
	ServerName  string       `json:"server_name"`
	Devices     []deviceView `json:"devices"`
}

func (m *Manager) pairingState() pairingView {
	v := pairingView{Devices: []deviceView{}}
	pm := m.opts.Pairing
	if pm == nil {
		return v
	}
	left := pm.Remaining()
	v.Open = left > 0
	v.SecondsLeft = int(left.Round(time.Second).Seconds())
	v.ServerName = pm.ServerName()
	for _, d := range pm.Devices() {
		v.Devices = append(v.Devices, deviceView{ID: d.ID, Name: d.Name, Note: d.Note, CreatedAt: d.CreatedAt})
	}
	return v
}

// handlePair 处理手机端扫码后的 POST /api/pair。
//
// 只接受 TLS：请求体里的 pairing_id + secret 来自二维码，明文传输等于把配对凭据广播。
func (m *Manager) handlePair(w http.ResponseWriter, r *http.Request) {
	pm := m.opts.Pairing
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method", "只支持 POST")
		return
	}
	if r.TLS == nil {
		writeError(w, http.StatusBadRequest, "tls_required", "配对请求必须使用 HTTPS")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, pairing.MaxPairBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_failed", "读取请求体失败")
		return
	}
	if len(body) > pairing.MaxPairBody {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "配对请求体过大")
		return
	}
	var req pairing.PairRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "配对请求不是合法 JSON")
		return
	}

	resp, err := pm.Pair(req)
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, resp)
	case errors.Is(err, pairing.ErrNoOffer):
		writeError(w, http.StatusGone, "offer_expired", "配对二维码已过期，请在电脑上刷新")
	case errors.Is(err, pairing.ErrBadSecret):
		writeError(w, http.StatusUnauthorized, "bad_secret", "配对密钥不匹配")
	case errors.Is(err, pairing.ErrBadRequest):
		writeError(w, http.StatusBadRequest, "bad_request", "配对请求字段不完整")
	case errors.Is(err, pairing.ErrTooMany):
		writeError(w, http.StatusConflict, "too_many", "已配对设备数量已达上限")
	default:
		m.logf("配对失败: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "配对失败")
	}
}

// handlePairSession 是设置页用的本机接口：查看/开启/关闭配对窗口。
func (m *Manager) handlePairSession(w http.ResponseWriter, r *http.Request) {
	if !m.localAuthed(r) || !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "forbidden", "只允许本机设置页操作")
		return
	}
	pm := m.opts.Pairing
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, m.pairingState())
	case http.MethodPost:
		// 默认复用未过期窗口（扫描中途刷新不会让码失效）；
		// ?rotate=1 是用户主动「换一张」，必须轮换密钥才有可见变化。
		open := pm.QR
		if r.URL.Query().Get("rotate") == "1" {
			open = pm.Rotate
		}
		if _, _, err := open(m.qrEndpoints()); err != nil {
			writeError(w, http.StatusInternalServerError, "qr_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, m.pairingState())
	case http.MethodDelete:
		pm.CloseOffer()
		writeJSON(w, http.StatusOK, m.pairingState())
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method", "只支持 GET / POST / DELETE")
	}
}

// handleDevices 管理已配对设备：DELETE 断开，PATCH 修改备注。
func (m *Manager) handleDevices(w http.ResponseWriter, r *http.Request) {
	if !m.localAuthed(r) || !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "forbidden", "只允许本机设置页操作")
		return
	}
	pm := m.opts.Pairing
	id := r.URL.Query().Get("id")

	switch r.Method {
	case http.MethodDelete:
		ok, err := pm.Revoke(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "revoke_failed", err.Error())
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "没有这台设备")
			return
		}
		writeJSON(w, http.StatusOK, m.pairingState())

	case http.MethodPatch:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1024))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_body", "读取请求体失败")
			return
		}
		var patch struct {
			Note *string `json:"note"`
		}
		if err := json.Unmarshal(body, &patch); err != nil || patch.Note == nil {
			writeError(w, http.StatusBadRequest, "bad_json", "请求体需为 {\"note\": \"...\"}")
			return
		}
		ok, err := pm.SetNote(id, *patch.Note)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "save_failed", err.Error())
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "没有这台设备")
			return
		}
		writeJSON(w, http.StatusOK, m.pairingState())

	default:
		w.Header().Set("Allow", "DELETE, PATCH")
		writeError(w, http.StatusMethodNotAllowed, "method", "只支持 DELETE / PATCH")
	}
}
