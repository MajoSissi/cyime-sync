package server

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"cyime-sync/internal/config"
)

// peerRequest 对应手机端 POST /api/peer 的请求体（WindowsPairingProtocol.registration）。
//
// 手机在 GET /api/status 看到 `push: true` 后才会发这个请求：把「往这里推」的地址、
// 证书指纹和一次授权令牌交给电脑。字段校验按手机端同一套规则做，宽了就收不到有效地址，
// 严了会让手机一直重试。
type peerRequest struct {
	V           int    `json:"v"`
	ID          string `json:"id"`
	ClientID    string `json:"client_id"`
	Name        string `json:"name"`
	Port        int    `json:"port"`
	Fingerprint string `json:"fingerprint"`
	Token       string `json:"token"`
}

// handlePeer 处理手机端登记推送地址。
//
// 与 /api/pair 一样只接受 TLS：请求体里带着手机发放的推送令牌，明文传输等于把令牌广播出去。
// 调用方身份来自 Bearer 令牌，所以登记只写到「当前这台设备」名下，请求体里的 client_id
// 不参与寻址（防止一台已配对设备把推送劫持到别处）。
func (m *Manager) handlePeer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method", "只支持 POST")
		return
	}
	if r.TLS == nil {
		writeError(w, http.StatusBadRequest, "tls_required", "推送登记必须使用 HTTPS")
		return
	}
	dev, ok := m.device(r)
	if !ok {
		writeUnauthorized(w)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_failed", "读取请求体失败")
		return
	}
	if len(body) > 4096 {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "推送登记请求体过大")
		return
	}
	var req peerRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "请求体不是合法 JSON")
		return
	}

	target := config.PushTarget{
		HostID:      req.ID,
		Host:        remoteHost(r),
		Port:        req.Port,
		Fingerprint: strings.ToLower(req.Fingerprint),
		Token:       req.Token,
		UpdatedAt:   time.Now(),
	}
	if req.V != 2 || !target.Valid() {
		writeError(w, http.StatusBadRequest, "bad_request", "推送地址字段不完整或格式不正确")
		return
	}

	saved, err := m.opts.Pairing.SetPushTarget(dev.ID, target)
	if err != nil {
		m.logf("保存推送地址失败: %v", err)
		writeError(w, http.StatusInternalServerError, "save_failed", "保存推送地址失败")
		return
	}
	if !saved {
		// 设备刚被撤销：令牌已经失效，让手机重新配对。
		writeUnauthorized(w)
		return
	}
	m.logf("设备 %s 登记推送地址 %s", deviceLabel(dev), target.Host)
	// 登记完立刻把当前剪贴板补推一次，不用等用户再复制。
	if m.opts.Relay != nil {
		m.opts.Relay.Kick()
	}
	writeJSON(w, http.StatusOK, map[string]any{"v": 2, "push": true})
}

// remoteHost 取请求来源 IP（不带端口），用于回推。IPv6 保持裸地址，拼 URL 时再加方括号。
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return strings.Trim(host, "[]")
}

func deviceLabel(dev config.PairedDevice) string {
	if dev.Note != "" {
		return dev.Note
	}
	if dev.Name != "" {
		return dev.Name
	}
	return dev.ID
}
