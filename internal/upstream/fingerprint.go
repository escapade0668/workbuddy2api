// fingerprint.go 出站客户端指纹：对齐官方 WorkBuddy 桌面端内置 CLI（2.165.0，2026-10-24 随 5.9.2 发布）。
//
// 指纹来源：2026-10-25 对官方 CLI（codebuddy-lite-wb.mjs 2.165.0）真实出站请求的端点覆盖抓包。
// 关键事实（与桌面端 VLM 转发路径不同，此前版本曾错对齐到后者）：
//   - UA 是 "CLI/<cli-ver> WorkBuddy/<cli-ver>"（CLI 在前，单一版本号，非桌面端 5.9.x）
//   - X-IDE-Type/Name 是 "CLI"，X-IDE-Version 是 CLI 版本
//   - X-Private-Data 是 "false"；chat 请求不带 Origin/Referer，也不带 acp-connection-id
//   - 携带 OpenAI JS SDK（Stainless）遥测头 x-stainless-*
//   - 会话关联 ID 是 uuidv7：X-Request-ID == X-Conversation-Message-ID，
//     X-Conversation-Request-ID == X-Root-Request-ID；另有 W3C baggage 头
//   - 大请求体 gzip 压缩（axios >1KB 阈值语义）
package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"runtime"
	"time"
)

// DefaultCLIVersion 官方桌面端 5.9.2 内置 CLI 版本（抓包实测）。CLI 升级后改这里或配 fingerprint.cli_version。
const DefaultCLIVersion = "2.165.0"

// Stainless SDK 遥测头的固定值（抓包实测：OpenAI JS SDK 6.25.0 / Node v22.22.2）。
const (
	stainlessLang           = "js"
	stainlessPackageVersion = "6.25.0"
	stainlessRuntime        = "node"
	stainlessRuntimeVersion = "v22.22.2"
)

// userAgent 组装出站 UA。格式照抄官方 CLI（抓包）：
//
//	CLI/2.165.0 WorkBuddy/2.165.0
//
// UserAgentOverride 非空时完全覆盖（逃生门）。
func (c *Client) userAgent() string {
	if c.UserAgentOverride != "" {
		return c.UserAgentOverride
	}
	v := c.CLIVersion
	if v == "" {
		v = DefaultCLIVersion
	}
	return "CLI/" + v + " WorkBuddy/" + v
}

// cliVersion X-IDE-Version 用的 CLI 版本（空回落默认）。
func (c *Client) cliVersion() string {
	if c.CLIVersion != "" {
		return c.CLIVersion
	}
	return DefaultCLIVersion
}

// stainlessOS 把 GOOS 映射为 Stainless 的 os 值（对齐 Node process.platform 的上报习惯）。
func stainlessOS() string {
	switch runtime.GOOS {
	case "windows":
		return "Windows"
	case "darwin":
		return "MacOS"
	case "linux":
		return "Linux"
	default:
		return runtime.GOOS
	}
}

// stainlessArch 把 GOARCH 映射为 Stainless 的 arch 值。
func stainlessArch() string {
	if runtime.GOARCH == "amd64" {
		return "x64"
	}
	return runtime.GOARCH
}

// applyStainlessHeaders OpenAI JS SDK（Stainless 生成代码）自带的遥测头，逐字对齐抓包。
func applyStainlessHeaders(req *http.Request) {
	req.Header.Set("x-stainless-arch", stainlessArch())
	req.Header.Set("x-stainless-lang", stainlessLang)
	req.Header.Set("x-stainless-os", stainlessOS())
	req.Header.Set("x-stainless-package-version", stainlessPackageVersion)
	req.Header.Set("x-stainless-retry-count", "0")
	req.Header.Set("x-stainless-runtime", stainlessRuntime)
	req.Header.Set("x-stainless-runtime-version", stainlessRuntimeVersion)
}

// applyTraceHeaders 链路追踪头，逐字段对齐官方 CLI：
//
//	traceparent       00-<trace32hex>-<span16hex>-01   （W3C Trace Context）
//	b3                <trace>-<span>-1-<parent>        （b3 单头约定）
//	X-B3-TraceId      <trace32hex>
//	X-B3-ParentSpanId <parent16hex>
//	X-B3-SpanId       <span16hex>
//	X-B3-Sampled      1
//	X-Trace-ID        <trace32hex>
func applyTraceHeaders(req *http.Request) {
	trace := randomHex(16)
	span := randomHex(8)
	parent := randomHex(8)
	req.Header.Set("traceparent", "00-"+trace+"-"+span+"-01")
	req.Header.Set("b3", trace+"-"+span+"-1-"+parent)
	req.Header.Set("X-B3-TraceId", trace)
	req.Header.Set("X-B3-ParentSpanId", parent)
	req.Header.Set("X-B3-SpanId", span)
	req.Header.Set("X-B3-Sampled", "1")
	req.Header.Set("X-Trace-ID", trace)
}

// randomHex 生成 n 字节 crypto/rand 随机的 hex 串（2n 字符）。
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newUUIDv7 生成 RFC 9562 UUID v7（48 位毫秒时间戳前缀 + 随机），对齐官方会话关联 ID 的格式
// （抓包值如 01a1710e-cdc1-7f23-...，时间有序）。不引入外部依赖。
func newUUIDv7() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0], b[1], b[2] = byte(ms>>40), byte(ms>>32), byte(ms>>24)
	b[3], b[4], b[5] = byte(ms>>16), byte(ms>>8), byte(ms)
	_, _ = rand.Read(b[6:])
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // RFC variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// newUUID 生成 RFC 4122 v4 UUID（旧用途保留）。
func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
