package winutil

import (
	"net"
	"sort"
	"strings"
)

// LocalIPv4s 返回可用于局域网访问的本机 IPv4 地址，私有网段优先。
// 用于设置页展示"安卓端应填写的地址"。
func LocalIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	type candidate struct {
		addr     string
		priority int
	}
	var out []candidate

	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ip := extractIPv4(a)
			if ip == "" || ip == "127.0.0.1" || ip == "0.0.0.0" {
				continue
			}
			prio := 3
			switch {
			case strings.HasPrefix(ip, "192.168."):
				prio = 0
			case strings.HasPrefix(ip, "10."):
				prio = 1
			case strings.HasPrefix(ip, "172."):
				prio = 2
			}
			out = append(out, candidate{addr: ip, priority: prio})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].priority != out[j].priority {
			return out[i].priority < out[j].priority
		}
		return out[i].addr < out[j].addr
	})

	seen := make(map[string]struct{}, len(out))
	res := make([]string, 0, len(out))
	for _, c := range out {
		if _, ok := seen[c.addr]; ok {
			continue
		}
		seen[c.addr] = struct{}{}
		res = append(res, c.addr)
	}
	return res
}

func extractIPv4(a net.Addr) string {
	var ip net.IP
	switch v := a.(type) {
	case *net.IPNet:
		ip = v.IP
	case *net.IPAddr:
		ip = v.IP
	default:
		return ""
	}
	if ip == nil {
		return ""
	}
	v4 := ip.To4()
	if v4 == nil {
		return ""
	}
	return v4.String()
}
