// Package pairing 实现 CyIME 电脑端配对协议 v2 的电脑侧：
// 自签 TLS 证书（手机按叶证书指纹固定信任）、限时配对窗口、逐设备访问令牌。
//
// 协议权威来源是手机端 Kotlin 实现（PairingQrDecoder / WindowsPairingProtocol /
// WindowsDevices）。二维码内容、字段顺序与二进制布局必须与它逐字节一致，
// 否则手机端会判定"不是 CyIME 配对码"。
package pairing

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

const (
	certName = "cert.pem"
	keyName  = "key.pem"

	// certValidity 证书有效期。手机端只校验指纹与有效期，长有效期省去续期逻辑；
	// 指纹变化等价于"换了一台电脑"，用户需重新扫码，因此必须持久化。
	certValidity = 10 * 365 * 24 * time.Hour
)

// loadOrCreateCert 读取既有自签证书；缺失或损坏时重新生成。
func loadOrCreateCert(dir string) (tls.Certificate, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("创建证书目录失败: %w", err)
	}
	certPath := filepath.Join(dir, certName)
	keyPath := filepath.Join(dir, keyName)

	if _, err := os.Stat(certPath); err == nil {
		if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
			return cert, nil
		}
		// 文件损坏：备份后重新生成，避免用户卡在无法启动的状态。
		_ = os.Rename(certPath, certPath+".bak")
		_ = os.Rename(keyPath, keyPath+".bak")
	}
	return generateCert(certPath, keyPath)
}

func generateCert(certPath, keyPath string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("生成私钥失败: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("生成序列号失败: %w", err)
	}
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "CyIME Sync", Organization: []string{"CyIME"}},
		NotBefore:             now.Add(-24 * time.Hour), // 容忍两端时钟偏差
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("签发证书失败: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("编码私钥失败: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("写入证书失败: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("写入私钥失败: %w", err)
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// fingerprint 返回叶证书 DER 的 SHA-256 小写十六进制。
// 手机端用它固定信任并在 TLS 握手中比对。
func fingerprint(cert tls.Certificate) string {
	if len(cert.Certificate) == 0 {
		return ""
	}
	sum := sha256.Sum256(cert.Certificate[0])
	return hex.EncodeToString(sum[:])
}
