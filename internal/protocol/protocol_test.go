package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDigestAndETag(t *testing.T) {
	text := "你好，CyIME 🎉"
	size, hash := Digest(text)
	if size != len([]byte(text)) {
		t.Fatalf("size 应为 UTF-8 字节数 %d，实际 %d", len([]byte(text)), size)
	}
	if len(hash) != 64 {
		t.Fatalf("hash 应为 64 位十六进制，实际 %q", hash)
	}
	if got := ETag(text); got != `"`+hash+`"` {
		t.Fatalf("ETag 格式不符: %s", got)
	}
	if ETag("") != "" {
		t.Fatal("空文本不应产生 ETag")
	}
	if ETag(text) == ETag(text+"x") {
		t.Fatal("内容变化后 ETag 必须变化")
	}
}

func TestBuildProfileShape(t *testing.T) {
	p := BuildProfile("abc", SourceDesktop)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if strings.Contains(s, "data_name") {
		t.Fatalf("响应体绝不能包含 data_name: %s", s)
	}
	if !strings.Contains(s, `"has_data":false`) {
		t.Fatalf("has_data 必须恒为 false: %s", s)
	}
	if !strings.Contains(s, `"type":"text"`) {
		t.Fatalf("type 必须恒为 text: %s", s)
	}
	if p.Size != 3 || p.Hash != DigestHash("abc") {
		t.Fatalf("size/hash 计算错误: %+v", p)
	}
}

// DigestHash 是测试辅助，避免重复写哈希逻辑。
func DigestHash(s string) string {
	_, h := Digest(s)
	return h
}

func TestParseRequestAccept(t *testing.T) {
	body := `{"type":"text","text":"  多行\r\n文本 ","size":0,"hash":"","source":"cyime-android","has_data":false}`
	text, err := ParseRequest([]byte(body))
	if err != nil {
		t.Fatalf("应接受合法请求: %v", err)
	}
	if text != "  多行\r\n文本 " {
		t.Fatalf("文本被意外修改: %q", text)
	}
}

func TestParseRequestRejects(t *testing.T) {
	big := strings.Repeat("a", MaxTextBytes+1)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"空请求体", ``, StatusBadRequest},
		{"非 JSON", `not-json`, StatusBadRequest},
		{"缺 text", `{"type":"text"}`, StatusBadRequest},
		{"type 非 text", `{"type":"image","text":"a"}`, StatusBadRequest},
		{"has_data 为真", `{"type":"text","text":"a","has_data":true}`, StatusBadRequest},
		{"出现 data_name", `{"type":"text","text":"a","data_name":"x.png"}`, StatusBadRequest},
		{"空文本", `{"text":""}`, StatusBadRequest},
		{"纯空白", `{"text":"  \r\n\t "}`, StatusBadRequest},
		{"含 NUL", "{\"text\":\"a\\u0000b\"}", StatusBadRequest},
		{"size 不符", `{"text":"abc","size":99}`, StatusBadRequest},
		{"hash 不符", `{"text":"abc","hash":"deadbeef"}`, StatusBadRequest},
		{"超长文本", `{"text":"` + big + `"}`, StatusTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseRequest([]byte(c.body))
			if err == nil {
				t.Fatal("期望被拒绝，实际通过")
			}
			pe, ok := err.(*Error)
			if !ok {
				t.Fatalf("错误类型应为 *Error，实际 %T", err)
			}
			if pe.Status != c.want {
				t.Fatalf("状态码期望 %d，实际 %d（%s）", c.want, pe.Status, pe.Message)
			}
		})
	}
}

func TestParseRequestTooLargeBody(t *testing.T) {
	body := `{"text":"` + strings.Repeat("a", MaxBodyBytes) + `"}`
	_, err := ParseRequest([]byte(body))
	pe, ok := err.(*Error)
	if !ok || pe.Status != StatusTooLarge {
		t.Fatalf("超大请求体应返回 413，实际 %v", err)
	}
}

func TestProtocolLimits(t *testing.T) {
	// 上限与手机端一致，改动会让两端行为分叉。
	if MaxTextBytes != 64*1024 || MaxBodyBytes != 256*1024 {
		t.Fatalf("协议上限被改动：%d / %d", MaxTextBytes, MaxBodyBytes)
	}
}
