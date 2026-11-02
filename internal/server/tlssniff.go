package server

import (
	"bufio"
	"crypto/tls"
	"net"
	"time"
)

// sniffListener 让同一个端口同时接受明文 HTTP 与 TLS：
//
//   - 本机浏览器打开设置页只发明文 HTTP，首字节是方法名（G/P/D...）；
//   - 手机端只发 HTTPS，首字节固定是 TLS 握手记录类型 0x16。
//
// 一个判断即可分流，用户和二维码只需要一个端口号。
type sniffListener struct {
	net.Listener
	conf *tls.Config
}

func (l *sniffListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		br := bufio.NewReaderSize(c, 1024)
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		first, err := br.Peek(1)
		_ = c.SetReadDeadline(time.Time{})
		if err != nil {
			c.Close()
			continue
		}
		pc := &prefixConn{Conn: c, r: br}
		if first[0] == 0x16 { // TLS handshake
			return tls.Server(pc, l.conf), nil
		}
		return pc, nil
	}
}

// prefixConn 把 peek 过的缓冲区接回读取路径，避免丢掉首个字节。
type prefixConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }
