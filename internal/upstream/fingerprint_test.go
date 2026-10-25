// fingerprint_test.go 出站指纹（CLI 2.165.0 抓包对齐）单测。
package upstream

import (
	"net/http"
	"regexp"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestUserAgentDefault(t *testing.T) {
	// 默认对齐官方 CLI 2.165.0 抓包：CLI token 在前，单一版本号。
	want := "CLI/2.165.0 WorkBuddy/2.165.0"
	if got := (&Client{}).userAgent(); got != want {
		t.Errorf("zero Client UA = %q, want %q", got, want)
	}
	if got := New().userAgent(); got != want {
		t.Errorf("New() UA = %q, want %q", got, want)
	}
}

func TestUserAgentConfigurable(t *testing.T) {
	c := &Client{CLIVersion: "2.200.0"}
	if got, want := c.userAgent(), "CLI/2.200.0 WorkBuddy/2.200.0"; got != want {
		t.Errorf("UA = %q, want %q", got, want)
	}
	c.UserAgentOverride = "CLI/2.63.2 CodeBuddy/2.63.2"
	if got := c.userAgent(); got != "CLI/2.63.2 CodeBuddy/2.63.2" {
		t.Errorf("override UA = %q", got)
	}
}

func TestChatHeadersFingerprint(t *testing.T) {
	c := New()
	req, _ := http.NewRequest(http.MethodPost, "https://upstream.test/v2/chat/completions", nil)
	c.ChatHeaders(req, &auth.Auth{AccessToken: "tok", UID: "u1", EnterpriseID: "ent1", Domain: "www.codebuddy.cn"}, "conv-123")

	for k, want := range map[string]string{
		// 官方 CLI 身份（抓包对齐：IDE 是 CLI，不是 WorkBuddy 桌面端）
		"X-IDE-Type":          "CLI",
		"X-IDE-Name":          "CLI",
		"X-IDE-Version":       "2.165.0",
		"X-Private-Data":      "false",
		"X-Agent-Intent":      "craft",
		"X-Agent-Purpose":     "conversation",
		"X-Agent-Runtime":     "other",
		"X-Agent-Type":        "main",
		"x-codebuddy-request": "1",
		// Stainless（OpenAI JS SDK）遥测头
		"x-stainless-lang":            "js",
		"x-stainless-package-version": "6.25.0",
		"x-stainless-runtime":         "node",
		"x-stainless-runtime-version": "v22.22.2",
		"x-stainless-retry-count":     "0",
		// 会话关联
		"X-Conversation-ID": "conv-123",
		"X-Product":         "SaaS",
		"Authorization":     "Bearer tok",
		"X-User-Id":         "u1",
		"X-Enterprise-Id":   "ent1",
		"X-Domain":          "www.codebuddy.cn",
		"User-Agent":        "CLI/2.165.0 WorkBuddy/2.165.0",
		"Accept":            "application/json",
	} {
		if got := req.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	// OS/arch 遥测头按运行平台映射（本机 windows/amd64）。
	if req.Header.Get("x-stainless-os") == "" || req.Header.Get("x-stainless-arch") == "" {
		t.Error("x-stainless-os/arch 缺失")
	}
	// 抓包的 ID 关系：msgID == X-Request-ID == X-Conversation-Message-ID，
	// reqID == X-Conversation-Request-ID == X-Root-Request-ID。
	if req.Header.Get("X-Request-ID") != req.Header.Get("X-Conversation-Message-ID") {
		t.Errorf("X-Request-ID != X-Conversation-Message-ID: %q vs %q",
			req.Header.Get("X-Request-ID"), req.Header.Get("X-Conversation-Message-ID"))
	}
	if req.Header.Get("X-Conversation-Request-ID") != req.Header.Get("X-Root-Request-ID") {
		t.Errorf("X-Conversation-Request-ID != X-Root-Request-ID: %q vs %q",
			req.Header.Get("X-Conversation-Request-ID"), req.Header.Get("X-Root-Request-ID"))
	}
	// 会话关联 ID 是 uuidv7（version nibble = 7）。
	uuidv7Re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for _, k := range []string{"X-Request-ID", "X-Conversation-Message-ID", "X-Conversation-Request-ID", "X-Root-Request-ID"} {
		if !uuidv7Re.MatchString(req.Header.Get(k)) {
			t.Errorf("%s = %q, want uuid v7", k, req.Header.Get(k))
		}
	}
	// baggage 携带同一组会话值。
	wantBaggage := "codebuddy.session_id=conv-123,codebuddy.conversation_request_id=" + req.Header.Get("X-Conversation-Request-ID")
	if got := req.Header.Get("baggage"); got != wantBaggage {
		t.Errorf("baggage = %q, want %q", got, wantBaggage)
	}
	// chat 不带 Origin/Referer 与 acp-connection-id（官方 CLI 抓包就不带）。
	for _, k := range []string{"Origin", "Referer", "acp-connection-id"} {
		if req.Header.Get(k) != "" {
			t.Errorf("chat 请求不应携带 %s", k)
		}
	}
	// 安全红线：chat 请求绝不带 X-Refresh-Token。
	if req.Header.Get("X-Refresh-Token") != "" {
		t.Error("chat 请求不应携带 X-Refresh-Token")
	}
	// convID 为空 → 自动生成 uuidv7（官方从不缺省该头）。
	req2, _ := http.NewRequest(http.MethodPost, "https://upstream.test/v2/chat/completions", nil)
	c.ChatHeaders(req2, &auth.Auth{AccessToken: "tok"}, "")
	if !uuidv7Re.MatchString(req2.Header.Get("X-Conversation-ID")) {
		t.Errorf("空会话键应生成 uuidv7: %q", req2.Header.Get("X-Conversation-ID"))
	}
}

func TestChatHeadersTrace(t *testing.T) {
	c := New()
	req, _ := http.NewRequest(http.MethodPost, "https://upstream.test/v2/chat/completions", nil)
	c.ChatHeaders(req, &auth.Auth{AccessToken: "tok"}, "")

	traceRe := regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanRe := regexp.MustCompile(`^[0-9a-f]{16}$`)
	trace := req.Header.Get("X-B3-TraceId")
	span := req.Header.Get("X-B3-SpanId")
	parent := req.Header.Get("X-B3-ParentSpanId")
	if !traceRe.MatchString(trace) {
		t.Errorf("X-B3-TraceId = %q, want 32hex", trace)
	}
	if !spanRe.MatchString(span) || !spanRe.MatchString(parent) {
		t.Errorf("X-B3-SpanId/ParentSpanId = %q/%q, want 16hex", span, parent)
	}
	if req.Header.Get("X-B3-Sampled") != "1" {
		t.Errorf("X-B3-Sampled = %q", req.Header.Get("X-B3-Sampled"))
	}
	if want := "00-" + trace + "-" + span + "-01"; req.Header.Get("traceparent") != want {
		t.Errorf("traceparent = %q, want %q", req.Header.Get("traceparent"), want)
	}
	if want := trace + "-" + span + "-1-" + parent; req.Header.Get("b3") != want {
		t.Errorf("b3 = %q, want %q", req.Header.Get("b3"), want)
	}
	if req.Header.Get("X-Trace-ID") != trace {
		t.Errorf("X-Trace-ID 与 TraceId 不一致: %q vs %q", req.Header.Get("X-Trace-ID"), trace)
	}
}

func TestBillingHeadersUA(t *testing.T) {
	c := &Client{UserAgentOverride: "TestUA/1.0"}
	req, _ := http.NewRequest(http.MethodPost, "https://upstream.test/v2/billing/meter/get-user-resource", nil)
	c.BillingHeaders(req, &auth.Auth{AccessToken: "tok", UID: "u1"})
	if req.Header.Get("User-Agent") != "TestUA/1.0" {
		t.Errorf("billing UA = %q", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("X-User-Id") != "u1" {
		t.Errorf("billing X-User-Id = %q", req.Header.Get("X-User-Id"))
	}
}

func TestNewUUIDv7Shape(t *testing.T) {
	u := newUUIDv7()
	parts := regexp.MustCompile(`-`).Split(u, -1)
	if len(parts) != 5 || len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		t.Fatalf("uuid 形状不对: %q", u)
	}
	if parts[2][0] != '7' {
		t.Errorf("version nibble = %q, want 7", parts[2][0])
	}
	// 时间有序：48 位毫秒时间戳前缀（前 12 hex）非递减。
	if u2 := newUUIDv7(); u2[:12] < u[:12] {
		t.Errorf("uuidv7 时间前缀回退: %q 之后生成 %q", u, u2)
	}
}
