// Package protocol 实现 CyIME 剪贴板同步协议的电脑端编解码（手机端扫码配对后使用）。
//
// 权威来源是安卓插件 main.lua：字段名、语义、状态码都以安卓端实际行为为准。
// 本包只做纯函数式计算与校验，不触碰剪贴板与网络。
package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// MaxTextBytes 单条文本上限（UTF-8 字节数）。
	MaxTextBytes = 64 * 1024
	// MaxBodyBytes 请求体上限（字节）。
	MaxBodyBytes = 256 * 1024
	// MaxSourceLen source 字段长度上限。
	MaxSourceLen = 128

	// TypeText 协议唯一允许的 profile 类型。
	TypeText = "text"

	// SourceDesktop 电脑端对外发送时固定使用的 source 标识。
	SourceDesktop = "cyime-windows"
	// SourceAndroid 安卓端默认 source 标识（缺失时用于事件标注）。
	SourceAndroid = "cyime-android"
)

// 状态码与安卓端 responseError 一一对应，语义不得改动。
const (
	StatusOK           = 200
	StatusNoContent    = 204
	StatusNotModified  = 304
	StatusBadRequest   = 400
	StatusUnauthorized = 401
	StatusForbidden    = 403
	StatusConflict     = 409
	StatusTooLarge     = 413
	StatusUnavailable  = 503
)

// 错误码用于响应体与日志，便于定位。
const (
	CodeBadRequest   = "bad_request"
	CodeUnauthorized = "unauthorized"
	CodeConflict     = "conflict"
	CodeTooLarge     = "too_large"
	CodeUnavailable  = "clipboard_unavailable"
)

// Profile 是请求体与响应体共用的 JSON 结构。
//
// 注意：结构体中刻意不存在 data_name 字段，且 HasData 无 omitempty —— 电脑端
// 输出里一旦出现 data_name 会被安卓端判为非法请求。
type Profile struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Size    int    `json:"size"`
	Hash    string `json:"hash"`
	Source  string `json:"source,omitempty"`
	HasData bool   `json:"has_data"`
}

// Error 表示可用 HTTP 状态码表达的协议错误。
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
}

func newError(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// BadRequest 构造 400。
func BadRequest(format string, args ...any) *Error {
	return newError(StatusBadRequest, CodeBadRequest, format, args...)
}

// TooLarge 构造 413。
func TooLarge(format string, args ...any) *Error {
	return newError(StatusTooLarge, CodeTooLarge, format, args...)
}

// Digest 返回 text 的 UTF-8 字节数与 SHA-256 小写十六进制。
// 服务端必须用它重新计算，绝不信任客户端传入的 size/hash。
func Digest(text string) (size int, hash string) {
	sum := sha256.Sum256([]byte(text))
	return len(text), hex.EncodeToString(sum[:])
}

// ETag 基于文本内容生成弱引用形式的强校验值：`"<sha256-hex>"`。
// 文本变化即变化；空文本返回空字符串（空剪贴板不产生 ETag）。
func ETag(text string) string {
	if text == "" {
		return ""
	}
	_, hash := Digest(text)
	return `"` + hash + `"`
}

// BuildProfile 构造电脑端响应体，size/hash 就地重算。
func BuildProfile(text, source string) Profile {
	size, hash := Digest(text)
	if source == "" {
		source = SourceDesktop
	}
	return Profile{
		Type:    TypeText,
		Text:    text,
		Size:    size,
		Hash:    hash,
		Source:  truncateRunes(source, MaxSourceLen),
		HasData: false,
	}
}

// incoming 是反序列化用的宽松结构：指针字段用于区分"缺失"与"零值"。
type incoming struct {
	Type     *string         `json:"type"`
	Text     *string         `json:"text"`
	Size     *int            `json:"size"`
	Hash     *string         `json:"hash"`
	Source   *string         `json:"source"`
	HasData  *bool           `json:"has_data"`
	DataName json.RawMessage `json:"data_name"`
}

// ParseRequest 解析并校验安卓端 PUT 请求体，返回待写入的文本。
//
// 校验顺序刻意贴合安卓端行为：
//  1. 非法 JSON / 缺字段 -> 400
//  2. type 必须为 text、has_data 必须为 false、不得出现 data_name -> 400
//  3. 空文本 / 纯空白 / 含 NUL / 非法 UTF-8 -> 400
//  4. 超过 64 KiB -> 413
//  5. 客户端若携带 hash/size，必须与本地重算一致，否则 400（不信任传入值）
func ParseRequest(body []byte) (string, error) {
	if len(body) == 0 {
		return "", BadRequest("请求体为空")
	}
	if len(body) > MaxBodyBytes {
		return "", TooLarge("请求体超过 %d 字节", MaxBodyBytes)
	}

	var in incoming
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(&in); err != nil {
		return "", BadRequest("请求体不是合法 JSON: %v", err)
	}
	if in.Text == nil {
		return "", BadRequest("缺少 text 字段")
	}
	if in.Type != nil && *in.Type != TypeText {
		return "", BadRequest("不支持的 type: %q", *in.Type)
	}
	if in.HasData != nil && *in.HasData {
		return "", BadRequest("has_data 必须为 false")
	}
	if len(in.DataName) > 0 && string(in.DataName) != "null" {
		return "", BadRequest("不允许携带 data_name 字段")
	}

	text := *in.Text
	if err := ValidateText(text); err != nil {
		return "", err
	}

	size, hash := Digest(text)
	// size/hash 只做交叉校验：客户端声明了就必须与本地重算一致；
	// 声明 0 / 空串视为"未提供"（部分客户端不填），服务端始终以自己算出的值为准。
	if in.Size != nil && *in.Size != 0 && *in.Size != size {
		return "", BadRequest("size 与文本实际字节数不符：声明 %d，实际 %d", *in.Size, size)
	}
	if in.Hash != nil && *in.Hash != "" && !strings.EqualFold(*in.Hash, hash) {
		return "", BadRequest("hash 与文本实际摘要不符")
	}
	return text, nil
}

// ValidateText 校验可同步文本内容。
func ValidateText(text string) error {
	if text == "" {
		return BadRequest("text 不能为空")
	}
	if !utf8.ValidString(text) {
		return BadRequest("text 不是合法 UTF-8")
	}
	if strings.IndexByte(text, 0) >= 0 {
		return BadRequest("text 不能包含 NUL 字符")
	}
	if len(text) > MaxTextBytes {
		return TooLarge("文本超过 %d 字节（当前 %d）", MaxTextBytes, len(text))
	}
	if strings.TrimSpace(text) == "" {
		return BadRequest("text 不能是纯空白内容")
	}
	return nil
}

// RequestSource 提取请求里的 source（仅用于日志元数据），非法时回退为空。
func RequestSource(body []byte) string {
	var in incoming
	if err := json.Unmarshal(body, &in); err != nil || in.Source == nil {
		return ""
	}
	src := strings.TrimSpace(*in.Source)
	if ValidateSource(src) != nil {
		return ""
	}
	return src
}

// ValidateSource 校验 source 合法性：可空，最长 128 字符。
func ValidateSource(src string) error {
	if src == "" {
		return nil
	}
	if utf8.RuneCountInString(src) > MaxSourceLen {
		return errors.New("source 超过 128 字符")
	}
	for _, r := range src {
		if r < 0x20 || r == 0x7f {
			return errors.New("source 含控制字符")
		}
	}
	return nil
}

// HashPrefix 返回 hash 前 8 位，用于事件流展示（不暴露完整正文指纹关联）。
func HashPrefix(hash string) string {
	if len(hash) > 8 {
		return hash[:8]
	}
	return hash
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	i, n := 0, 0
	for idx := range s {
		if n == max {
			i = idx
			break
		}
		n++
	}
	return s[:i]
}
