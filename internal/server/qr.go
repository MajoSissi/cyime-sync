package server

import (
	"net"
	"net/http"
	"strconv"

	qrcode "github.com/skip2/go-qrcode"

	"cyime-sync/internal/pairing"
	"cyime-sync/internal/winutil"
)

// qrPixelSize 二维码分辨率。640px 让紧凑格式（约 53 模块）每模块 12px，
// 手机在 30cm 内一次扫中；页面按 168px 显示时仍留有余量。
const qrPixelSize = 640

// handleQR 输出扫码配对二维码。
//
// 二维码内含一次性配对密钥，等价于"允许扫码者读写本机剪贴板"，
// 因此只允许本机设置页携带一次性令牌获取，不能匿名公开。
func (m *Manager) handleQR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "method", "只支持 GET")
		return
	}
	if !m.localAuthed(r) {
		writeError(w, http.StatusForbidden, "forbidden", "二维码只允许本机设置页获取")
		return
	}
	text, expires, err := m.opts.Pairing.QR(m.qrEndpoints())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "qr_failed", "生成二维码失败: "+err.Error())
		return
	}
	png, err := qrcode.Encode(text, qrcode.Medium, qrPixelSize)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "qr_failed", "生成二维码失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	w.Header().Set("X-Cyime-Pair-Expires", strconv.FormatInt(expires.Unix(), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}

// qrEndpoints 返回二维码里要写入的 HTTPS 地址（IPv4，私有网段优先）。
//
// 手机端只接受 https + 数字 IP + 私有/环路地址，域名和 0.0.0.0 都会被判为非法。
func (m *Manager) qrEndpoints() []pairing.Endpoint {
	cfg := m.opts.Config.Get()
	var hosts []string
	switch cfg.ListenAddress {
	case "", "0.0.0.0", "::":
		hosts = siteLocalIPv4s()
	default:
		if ip := net.ParseIP(cfg.ListenAddress); ip != nil && ip.To4() != nil {
			hosts = []string{ip.String()}
		}
	}
	if len(hosts) == 0 {
		hosts = []string{"127.0.0.1"}
	}
	seen := make(map[string]bool, len(hosts))
	out := make([]pairing.Endpoint, 0, len(hosts))
	for _, h := range hosts {
		if seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, pairing.Endpoint{Host: h, Port: cfg.Port})
		if len(out) == 8 { // 手机端最多接受 8 个地址
			break
		}
	}
	return out
}

// siteLocalIPv4s 过滤出私有网段地址；没有时退回回环地址。
func siteLocalIPv4s() []string {
	var out []string
	for _, addr := range winutil.LocalIPv4s() {
		ip := net.ParseIP(addr)
		if ip == nil || ip.To4() == nil {
			continue
		}
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			out = append(out, addr)
		}
	}
	return out
}
