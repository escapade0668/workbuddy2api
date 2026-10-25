// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
//
// Chat 头逐字对齐 2026-10-25 抓包的官方 CLI 2.165.0 真实出站请求
// （见 fingerprint.go 头注释）；其余三类沿用既有行为 + 新 UA。
package upstream

import (
	"net/http"

	"workbuddy2api/internal/auth"
)

const (
	// originRefererGlobal Global 账号必须带 workbuddy.ai 的 Origin —— 上游会校验来源，
	// 用 CN origin 请求 Global 端点会被判为跨站请求而拒绝。
	originRefererGlobal = "https://www.workbuddy.ai"
	// originRefererCNDefault CN realm 默认 Origin（endpoint_preset=workbuddy 时被覆盖）。
	originRefererCNDefault = "https://www.codebuddy.cn"
)

// originRefererFor 按账号 region 返回 Origin/Referer 基址（chat 请求不用，见 ChatHeaders）。
func (c *Client) originRefererFor(a *auth.Auth) string {
	if isGlobalRealm(a) {
		return originRefererGlobal
	}
	if c.OriginCN != "" {
		return c.OriginCN
	}
	return originRefererCNDefault
}

// CommonHeaders 非 chat API（models/refresh 等）共享的请求头。
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := c.originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.userAgent())
}

// ChatHeaders 官方 CLI 2.165.0 的 chat 请求头（抓包逐字对齐）。
//
// convID = 会话键（请求体 conversationId，与粘性路由同源）；空串时生成随机 uuidv7
// ——官方从不缺省该头。ID 关系照抄抓包：msgID == X-Request-ID == X-Conversation-Message-ID，
// reqID == X-Conversation-Request-ID == X-Root-Request-ID；baggage 携带同一组值。
// 注意：chat 不带 Origin/Referer 与 acp-connection-id（官方 CLI 就不带）。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, convID string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	applyStainlessHeaders(req)

	if convID == "" {
		convID = newUUIDv7()
	}
	reqID := newUUIDv7()
	msgID := newUUIDv7()
	req.Header.Set("X-Conversation-ID", convID)
	req.Header.Set("X-Conversation-Request-ID", reqID)
	req.Header.Set("X-Conversation-Message-ID", msgID)
	req.Header.Set("X-Root-Request-ID", reqID)
	req.Header.Set("X-Request-ID", msgID)
	req.Header.Set("baggage", "codebuddy.session_id="+convID+",codebuddy.conversation_request_id="+reqID)

	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Runtime", "other")
	req.Header.Set("X-Agent-Type", "main")
	req.Header.Set("X-IDE-Type", "CLI")
	req.Header.Set("X-IDE-Name", "CLI")
	req.Header.Set("X-IDE-Version", c.cliVersion())
	req.Header.Set("X-Private-Data", "false")
	req.Header.Set("x-codebuddy-request", "1")

	if a.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("User-Agent", c.userAgent())
	applyTraceHeaders(req)
}

// BillingHeaders billing 接口请求头。UA 与 chat 同源。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("User-Agent", c.userAgent())
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
}
