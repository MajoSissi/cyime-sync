package pairing

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"time"

	"cyime-sync/internal/config"
)

// QRPrefix 是手机端识别配对二维码的前缀。
//
// 手机端只接受 "cyime://pair?v=2&c=" (紧凑二进制) 或
// "cyime://pair?v=2&data=" (旧 JSON)；其它内容一律提示"不是 CyIME 配对码"。
const QRPrefix = "cyime://pair?v=2&c="

// qrMaxEndpoints 二维码里最多放几个地址（手机端限制 1..8）。
const qrMaxEndpoints = 8

// encodeQR 按手机端 WindowsPairingProtocol.compactOffer 的字节布局打包：
//
//	[0]            版本 = 2
//	[1]            地址个数 n
//	重复 n 次      4 字节 IPv4 + 2 字节端口（大端）
//	16 字节        server_id (UUID 原始字节)
//	32 字节        证书 DER 的 SHA-256
//	16 字节        pairing_id (UUID 原始字节)
//	32 字节        配对密钥
//	8 字节         过期时间 (unix 秒，大端)
//
// 总长固定 106 + 6n 字节；手机端据此校验，多一字节都会被判为非法。
func encodeQR(serverID, fpHex string, eps []Endpoint, pairingID string, secret []byte, expires time.Time) (string, error) {
	if len(eps) == 0 || len(eps) > qrMaxEndpoints {
		return "", fmt.Errorf("地址数量必须在 1-%d 之间，当前 %d", qrMaxEndpoints, len(eps))
	}
	if len(secret) != 32 {
		return "", fmt.Errorf("配对密钥长度非法: %d", len(secret))
	}
	sid, err := config.ParseUUID(serverID)
	if err != nil {
		return "", err
	}
	pid, err := config.ParseUUID(pairingID)
	if err != nil {
		return "", err
	}
	fp, err := hex.DecodeString(fpHex)
	if err != nil || len(fp) != 32 {
		return "", fmt.Errorf("证书指纹非法: %q", fpHex)
	}

	buf := make([]byte, 0, 106+6*len(eps))
	buf = append(buf, 2, byte(len(eps)))
	for _, ep := range eps {
		ip := net.ParseIP(ep.Host).To4()
		if ip == nil {
			return "", fmt.Errorf("二维码只支持 IPv4 地址: %q", ep.Host)
		}
		if ep.Port < 1 || ep.Port > 65535 {
			return "", fmt.Errorf("端口非法: %d", ep.Port)
		}
		buf = append(buf, ip...)
		buf = append(buf, byte(ep.Port>>8), byte(ep.Port))
	}
	buf = append(buf, sid[:]...)
	buf = append(buf, fp...)
	buf = append(buf, pid[:]...)
	buf = append(buf, secret...)
	buf = binary.BigEndian.AppendUint64(buf, uint64(expires.Unix()))

	if want := 106 + 6*len(eps); len(buf) != want {
		return "", fmt.Errorf("二维码负载长度异常: %d != %d", len(buf), want)
	}
	return QRPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}
