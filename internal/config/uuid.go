package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewUUID 生成 v4 UUID（小写、带短横）。手机端用 UUID.fromString 解析，格式必须标准。
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败属于系统级异常；用固定前缀的退化值仍保持格式合法。
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return formatUUID(b)
}

// IsUUID 判断字符串是否为标准 8-4-4-4-12 形式。
func IsUUID(s string) bool {
	_, err := ParseUUID(s)
	return err == nil
}

// ParseUUID 解析标准 UUID 为其 16 字节大端表示。
func ParseUUID(s string) ([16]byte, error) {
	var out [16]byte
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return out, fmt.Errorf("不是合法 UUID: %q", s)
	}
	hexOnly := s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:36]
	if _, err := hex.Decode(out[:], []byte(hexOnly)); err != nil {
		return out, fmt.Errorf("不是合法 UUID: %q", s)
	}
	return out, nil
}

func formatUUID(b [16]byte) string {
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}
