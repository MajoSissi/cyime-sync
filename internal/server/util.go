package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"cyime-sync/internal/config"
	"cyime-sync/internal/protocol"
	"cyime-sync/internal/winutil"
)

// constantTimeEqual 常量时间比较，避免按字节泄露本地令牌。
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// device 解析 Authorization: Bearer 并校验设备令牌。
// 失败一律 401，不再区分"令牌无效"与"未提供"，避免给探测者额外信息。
func (m *Manager) device(r *http.Request) (config.PairedDevice, bool) {
	token, ok := bearerToken(r)
	if !ok {
		return config.PairedDevice{}, false
	}
	return m.opts.Pairing.Authenticate(token)
}

// bearerToken 取出设备令牌；不是 Bearer 形式时返回 false。
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 8 || !strings.EqualFold(h[:7], "Bearer ") {
		return "", false
	}
	return strings.TrimSpace(h[7:]), true
}

// writeUnauthorized 输出 401。协议 v2 用纯 JSON，不发 Basic 质询。
func writeUnauthorized(w http.ResponseWriter) {
	writeError(w, protocol.StatusUnauthorized, protocol.CodeUnauthorized, "设备令牌无效或已被撤销")
}

// matchETag 判断 If-None-Match 是否命中当前 ETag。
// 兼容逗号分隔多值、W/ 弱校验前缀与 "*"。
func matchETag(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" || etag == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "W/")
		if part == etag {
			return true
		}
	}
	return false
}

// jsonError 是错误响应体。手机端只看状态码，正文用于本地排障。
type jsonError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// 客户端已断开，无法再做任何补救。
		return
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, jsonError{Code: code, Message: message})
}

// writeProtocolError 把协议错误映射为响应。
func writeProtocolError(w http.ResponseWriter, err error) {
	var perr *protocol.Error
	if ok := asProtocolError(err, &perr); ok {
		writeError(w, perr.Status, perr.Code, perr.Message)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal", err.Error())
}

func asProtocolError(err error, target **protocol.Error) bool {
	if e, ok := err.(*protocol.Error); ok {
		*target = e
		return true
	}
	return false
}

// firstLocalIPv4 返回首个可用的局域网 IPv4 地址。
func firstLocalIPv4() string {
	addrs := winutil.LocalIPv4s()
	if len(addrs) == 0 {
		return ""
	}
	return addrs[0]
}
