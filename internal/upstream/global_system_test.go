package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// outboundMessages 发一次 ChatStream，返回实际发给上游的 messages。
func outboundMessages(t *testing.T, a *auth.Auth, body string) []map[string]any {
	t.Helper()
	var sent []byte
	c := New()
	c.ChatBaseCN, c.ChatBaseGlobal = "https://cn.fake", "https://global.fake"
	c.HTTP = &http.Client{Transport: v3RoundTrip(func(r *http.Request) (*http.Response, error) {
		sent, _ = io.ReadAll(r.Body)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
	})}
	c.ChatHTTP = c.HTTP
	rc, status, _, err := c.ChatStream(a, []byte(body))
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	rc.Close()
	var out struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(sent, &out); err != nil {
		t.Fatalf("outbound not json: %s", sent)
	}
	return out.Messages
}

// 国际版（workbuddy.ai）要求首条消息是 system，否则 400 code=11128
// "first message is not system prompt"（2026-10-05 实测；空串 system 即可通过）。
// 网关此前原样透传，国际版账号上没有 system 的请求全部失败、被静默轮转到国内版账号。
func TestChatStreamGlobalPrependsEmptySystem(t *testing.T) {
	msgs := outboundMessages(t, globalAcct, `{"model":"hy3","messages":[{"role":"user","content":"hi"}]}`)
	if len(msgs) != 2 || msgs[0]["role"] != "system" || msgs[0]["content"] != "" || msgs[1]["content"] != "hi" {
		t.Fatalf("国际版应在首位补空 system: %v", msgs)
	}
}

// system 不在首位（实测同样 11128）：补一条空 system 在最前，原有消息顺序与内容不动。
func TestChatStreamGlobalSystemNotFirst(t *testing.T) {
	msgs := outboundMessages(t, globalAcct, `{"model":"hy3","messages":[{"role":"user","content":"hi"},{"role":"system","content":"S"}]}`)
	if len(msgs) != 3 || msgs[0]["role"] != "system" || msgs[0]["content"] != "" || msgs[2]["content"] != "S" {
		t.Fatalf("got %v", msgs)
	}
}

// 已有首条 system 的不改；国内版一律不改。
func TestChatStreamLeavesValidOrCNUntouched(t *testing.T) {
	msgs := outboundMessages(t, globalAcct, `{"model":"hy3","messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}]}`)
	if len(msgs) != 2 || msgs[0]["content"] != "S" {
		t.Fatalf("已有首条 system 不应再补: %v", msgs)
	}
	msgs = outboundMessages(t, cnAcct, `{"model":"hy3","messages":[{"role":"user","content":"hi"}]}`)
	if len(msgs) != 1 || msgs[0]["role"] != "user" {
		t.Fatalf("国内版不应改动: %v", msgs)
	}
}
