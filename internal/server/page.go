package server

import (
	"bytes"
	"net/http"

	"cyime-sync/web"
)

// 页面占位符：服务端按访问来源注入本地令牌。
const (
	placeholderToken = "__CYIME_TOKEN__"
	placeholderLocal = "__CYIME_LOCAL__"
)

// handlePage 渲染单页设置界面。
//
// 只有来自 127.0.0.1 的访问才注入令牌；局域网其它主机打开时页面为只读提示状态，
// 这样即使别人能访问设置页，也拿不到能改配置的凭据。
func (m *Manager) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	raw, err := web.FS.ReadFile("index.html")
	if err != nil {
		http.Error(w, "页面资源缺失", http.StatusInternalServerError)
		return
	}

	local := isLoopback(r)
	token := ""
	localFlag := "0"
	if local {
		token = m.token
		localFlag = "1"
	}

	page := bytes.ReplaceAll(raw, []byte(placeholderToken), []byte(token))
	page = bytes.ReplaceAll(page, []byte(placeholderLocal), []byte(localFlag))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page)
}

// serveAsset 从内嵌文件系统输出静态资源。
func (m *Manager) serveAsset(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := web.FS.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}
