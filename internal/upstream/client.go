// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrHardCredit                 // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate                   // 429 软限流 → 短冷却
	ErrSessionDead                // 401 + 12153 offline session 失效 → 禁用
	ErrNotFound                   // 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）
	ErrServer                     // 5xx 上游故障
	ErrBadParams                  // 请求体解析失败（400 + Unmarshal chat params failed / 11101）→ 不罚账号，仍轮转
	ErrClient                     // 其他 4xx / 业务错误
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrBadParams:
		return "bad_params"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardMarkers 余额不足关键词（小写比较 + 中文原文比较双通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// softRateMarkers 限流/节流关键词（小写比较 + 中文原文比较双通道）。
// 上游在状态码非 429 时也会返回限流语义（如 200 + code 11140
// "The model provider is rate-limiting requests."、400 + "rate limit"），
// 此类响应若不识别，账号既不被冷却也不喂熔断，下次请求仍会被选中（issue #28）。
//
// 词表按子串匹配，宁缺毋滥：只收录明确指向「请求速率/模型用量被节流」的措辞。
// 连字符形式（rate-limiting / rate-limited）需单列——Contains 不跨 '-'。
// "too many" 会命中 "too many tokens" 这类客户端参数错误，代价是该号被软冷却
// 一个 SoftCooldown（默认 60s）后自愈，远小于漏判限流导致反复选中同一号的代价。
var softRateMarkers = []string{
	"rate limit", // rate limit / rate limits / rate limiting
	"rate-limiting",
	"rate-limited",
	"too many requests",
	"too many",
	"usage limit", // usage limit reached / model usage limit exceeded（用量节流，非计费余额）
	"请求过于频繁", "限流",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// modelRateLimitCode 明确指向「模型级 429 限流」的业务 code。
// 上游用它表达"该模型的使用量超限"（code 6004，msg 带「将在 … 重置」），
// 与账号级限流不同：切到别的模型即可继续用，不该把整个账号拉黑。
const modelRateLimitCode = "6004"

// softRateResetLoc 上游 429 6004 文案中的重置时间固定按 UTC+8 解释（上游文案如此，
// 与中国区无夏令时一致）。
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// SoftRateResetLoc 暴露重置时间的时区（供测试/文档引用）。
func SoftRateResetLoc() *time.Location { return softRateResetLoc }

// softRateResetRe 匹配「将在 … 重置」，捕获中间的时间串。
const softRateResetRe = `将在 (.+?) 重置`

// softRateTimeLayout 上游重置时间的格式（无时区后缀；时区固定 UTC+8）。
const softRateTimeLayout = "2006-01-02 15:04:05"

// IsModelRateLimit 报告 body 是否明确指向模型级限流（业务 code 6004）。
// 用于区分"账号级软限流"（按账号冷却）与"模型级用量限流"（切模型即可用）。
func IsModelRateLimit(body string) bool {
	// `"code":6004` / `"code": 6004` / `"code":"6004"` 均可命中（JSON 空格容差）。
	re := regexp.MustCompile(`"code"\s*:\s*"?` + modelRateLimitCode + `"?`)
	return re.MatchString(body)
}

// ParseSoftRateReset 从 429 body 解析「将在 … 重置」时间（上游 UTC+8 文案）。
// 成功返回解析出的**墙钟时刻**（按 UTC+8 解释），失败返回零值 + false。
//
// 内部先判 IsModelRateLimit：非模型级限流（非 6004）即使带"重置"字样也不返回——
// 该重置无冷却语义（如 11140 的通用限流提示），解析出来反而会错误收窄冷却。
func ParseSoftRateReset(body string) (time.Time, bool) {
	if !IsModelRateLimit(body) {
		return time.Time{}, false
	}
	re := regexp.MustCompile(softRateResetRe)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return time.Time{}, false
	}
	ts := strings.TrimSpace(m[1])
	ts = strings.TrimSuffix(ts, " UTC+8") // 去掉后缀，固定按 softRateResetLoc 解释
	t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
//
// 判定顺序自「严」到「宽」，每层的先后都有语义依据：
//  1. 402 / hardMarkers —— 计费额度耗尽，最严、最不可自愈，必须最先判。
//     "quota exceeded" 语义跨计费/限流两界，历史归 hard_credit，本次保持不变
//     （issue #28 已记录该反向误判风险，待上游原始响应确认后再定）。
//  2. sessionDeadMarkers —— 需要人工重登的终态。若 401 body 同时含 "12153" 与
//     "rate limit"（如网关错误页混排），归 session_dead：短冷却救不活失效 session，
//     误判为限流会让该死号留在池中反复被选中；且此层 marker 是精确词（12153 等），
//     比限流层的大范围子串更具体，具体优先于宽泛。
//  3. softRateMarkers —— 非 429 状态码携带限流文案（issue #28 修复点）。
//     位于此处可覆盖 200/400/403/5xx 各状态码；429 且 body 含文案时在此短路，
//     结果同为 soft_rate，与下一层一致。
//  4. status==429 —— body 无文案时的兜底识别。
//  5. 404 / 5xx / 其他 4xx —— 与限流无关的常规分类。
//
// badParamsMarkers 请求体解析失败关键词（issue #41 连带）：HTTP 400 + 上游
// "Unmarshal chat params failed..."（code 11101）。这是"发给上游的 body 有问题"，
// 与账号健康无关——不罚号，但仍轮转（不同账号可能有不同模型权限，值得再试）。
var badParamsMarkerMsg = "Unmarshal chat params failed"
var badParamsMarkerCode = `"code":11101`

func Classify(status int, body string) ErrKind {
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	for _, m := range softRateMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrSoftRate
		}
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		// 请求体解析失败（HTTP 400 + Unmarshal chat params failed / code 11101）：
		// 这是"发给上游的 body 有问题"。网关侧截断已由 413 消灭（issue #41），
		// 剩余来源是客户端 JSON 本身畸形——换了账号照样 400，不该罚号
		// （白白冷却好号）。判在通用 4xx 之前，避免与限流文案误判混淆。
		if strings.Contains(body, badParamsMarkerMsg) || strings.Contains(body, badParamsMarkerCode) {
			return ErrBadParams
		}
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardMarkers 捕获。
	return ErrNone
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client

	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），首字节由
	// Transport.ResponseHeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	// 与 HTTP 共享同一个 *http.Transport 实例，连接池不重复。
	ChatHTTP *http.Client

	// HeaderTimeout 聊天 SSE 首字节前（响应头）超时；<=0 表示未设置（回落 HTTP.Timeout）。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲超时；<=0 表示禁用空闲监控。
	IdleTimeout time.Duration

	// effortsMu/efforts 缓存各模型 supportedEfforts（FetchModels 刷新），供请求体 effort 降级。
	effortsMu sync.RWMutex
	efforts   map[string][]string

	// SanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	SanitizeFingerprints bool

	// —— 客户端指纹（对齐官方 CLI 2.165.0 抓包，见 fingerprint.go / headers.go）——
	// CLIVersion 组装 UA 与 X-IDE-Version；UserAgentOverride 非空时完全覆盖 UA
	//（逃生门）。零值回落内置默认。
	CLIVersion        string
	UserAgentOverride string
	// OriginCN CN realm 的 Origin/Referer 基址；空回落 codebuddy.cn
	// （endpoint_preset=workbuddy 时由 main 注入 workbuddy.cn）。
	OriginCN string

	// ChatBaseCN/BillingBaseCN CN realm（copilot.tencent.com / codebuddy.cn）。
	ChatBaseCN    string
	BillingBaseCN string
	// ChatBaseGlobal/BillingBaseGlobal GLOBAL realm（workbuddy.ai）。
	// 按账号 domain 选择：见 chatBase()/billingBase()。
	ChatBaseGlobal    string
	BillingBaseGlobal string
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		// 聊天 SSE 首字节前硬上限（对短 RPC 无实际影响：其总时长 120s 更先到期）。
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:                 &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:             &http.Client{Timeout: 0, Transport: tr}, // 无总时长；首字节由 ResponseHeaderTimeout 管
		SanitizeFingerprints: true,
		CLIVersion:           DefaultCLIVersion,
		ChatBaseCN:           "https://copilot.tencent.com",
		BillingBaseCN:        "https://www.codebuddy.cn",
		ChatBaseGlobal:       "https://www.workbuddy.ai",
		BillingBaseGlobal:    "https://www.workbuddy.ai",
	}
}

// chatHTTP 返回聊天专用 client；未设置（如测试只注入 HTTP）时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

// isGlobalRealm 判断账号属于 GLOBAL realm（workbuddy.ai）还是 CN realm。
//
// 判据是账号凭证里的 domain —— cmd/login 会把上游返回的 domain 落盘，
// Global 登录得到的 domain 是 www.workbuddy.ai。
// 兼容两种写法：完整域名（www.workbuddy.ai）与裸域（workbuddy.ai）。
// domain 为空（老凭证/CN 凭证）一律按 CN 处理，保持既有行为不变。
func isGlobalRealm(a *auth.Auth) bool {
	if a == nil {
		return false
	}
	d := strings.ToLower(strings.TrimSpace(a.Domain))
	if d == "" {
		return false
	}
	// 去掉可能的 scheme 前缀，避免 "https://www.workbuddy.ai" 之类写法漏判。
	d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// chatBase 按账号 region 返回聊天基址（Global → workbuddy.ai，其余 → CN）。
func (c *Client) chatBase(a *auth.Auth) string {
	if isGlobalRealm(a) && c.ChatBaseGlobal != "" {
		return c.ChatBaseGlobal
	}
	return c.ChatBaseCN
}

// prepareBody 组装出站请求体（脱敏开关由 Client.SanitizeFingerprints 控制）。
func (c *Client) prepareBody(body []byte) []byte {
	return PrepareBodyOptWithEfforts(body, c.SanitizeFingerprints, c.effortsSnapshot())
}

// effortsSnapshot 返回 effort 能力缓存副本；nil 表示未知（透传不降级）。
func (c *Client) effortsSnapshot() map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	if len(c.efforts) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(c.efforts))
	for k, v := range c.efforts {
		cp[k] = v
	}
	return cp
}

// billingBase 按账号 region 返回计费基址（Global → workbuddy.ai，其余 → CN）。
func (c *Client) billingBase(a *auth.Auth) string {
	if isGlobalRealm(a) && c.BillingBaseGlobal != "" {
		return c.BillingBaseGlobal
	}
	return c.BillingBaseCN
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。全程持 a 锁，防止并发 SaveAtomic 读半更新 token。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	url := c.chatBase(a) + "/v2/plugin/auth/token/refresh"
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	c.RefreshHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify(status, string(body))）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	url := c.chatBase(a) + "/v2/chat/completions"
	payload := c.prepareBody(body)
	if isGlobalRealm(a) {
		payload = ensureLeadingSystem(payload)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, nil, err
	}
	// 会话关联头（官方每请求必带）：会话键与粘性路由同源（请求体 conversationId）。
	convID := session.ExtractKey(body)
	c.ChatHeaders(req, a, convID)
	// 大请求体 gzip（对齐官方 CLI 的 axios >1KB 压缩阈值语义）。
	if len(payload) > 1024 {
		var zb bytes.Buffer
		zw := gzip.NewWriter(&zb)
		_, _ = zw.Write(payload)
		_ = zw.Close()
		payload = zb.Bytes()
		req.Header.Set("Content-Encoding", "gzip")
		req.ContentLength = int64(len(payload))
		req.Body = io.NopCloser(bytes.NewReader(payload))
	}
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		cancel()
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
			a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）；
	// IdleTimeout<=0 时 monitorBody 原样返回底流、无人调 cancel——可接受：
	// ctx 无 deadline 无 goroutine，连接由 resp.Body.Close 正常清理。
	return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

// ensureLeadingSystem 保证 messages 首条是 system：不是则在最前补一条空 system，其余原样。
//
// 国际版（workbuddy.ai）要求首条消息是 system，否则 400 code=11128
// "first message is not system prompt"（被上游当安全策略拦截）。2026-10-05 实测：
// 空串 system 即可通过，对对话语义无影响；system 不在首位同样被拒，故补在最前而不是挪动原消息。
// 网关此前原样透传：国际版账号上不带 system 的请求全部失败，又被当普通 4xx 静默轮转到国内版。
// 解析失败（非 JSON / 无 messages）时原样返回，交给上游报错。
func ensureLeadingSystem(body []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(obj["messages"], &msgs); err != nil || len(msgs) == 0 {
		return body
	}
	var first struct {
		Role string `json:"role"`
	}
	if json.Unmarshal(msgs[0], &first) == nil && first.Role == "system" {
		return body
	}
	msgs = append([]json.RawMessage{json.RawMessage(`{"role":"system","content":""}`)}, msgs...)
	raw, err := json.Marshal(msgs)
	if err != nil {
		return body
	}
	obj["messages"] = raw
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// ModelInfo 动态模型信息（上游 data.models 条目，只取网关用得到的字段）。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64    // = maxInputTokens
	MaxTokens     int64    // = maxOutputTokens
	Efforts       []string // reasoning.supportedEfforts（空=未知/固定档）

	// 以下为模型目录元信息，原样透出到 /v1/models（缺省即上游未给）。
	Description       string // descriptionZh，缺省回落 descriptionEn
	Credits           string // 积分倍率原文，如 "x0.79" / "x0.19 credits"；空 = 上游未标
	Vendor            string
	Tags              []string
	IsDefault         bool
	OnlyReasoning     bool
	SupportsImages    bool
	SupportsReasoning bool
	SupportsToolCall  bool
	MaxAllowedSize    int64
	ReasoningEffort   string // reasoning.defaultEffort，缺省回落 reasoning.effort
}

// 模型目录端点。
const (
	// endpointV3Config 首选：两个域都可用，且带倍率等元信息。
	endpointV3Config = "/v3/config"
	// endpointConsoleModels 旧端点，仅作回落：2026-10-05 实测国际版恒返回 500 HTML。
	endpointConsoleModels = "/console/enterprises/personal/models"
)

// FetchModels 拉取账号**所在域**的模型目录（只含 cli agent 可调用的模型，按其顺序）。
//
// 首选 /v3/config：旧端点 /console/enterprises/personal/models 在国际版（workbuddy.ai）
// 恒返回 500，国际版账号一直拿不到动态目录、静默回落静态表。/v3/config 两个域都正常。
//
// 必须用网关自己的 CLI 头取（CommonHeaders）：同一账号用桌面端 UA 取到的列表与 CLI 身份
// 可调用的并不一致（实测 gpt-6-luna 在桌面端列表里，CLI 身份调用报 "service info not found"）；
// CLI UA 列表里的模型实测都能调通。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	out, err := c.fetchModelsFrom(a, endpointV3Config)
	if err != nil {
		log.Printf("fetch models uid=%s: %s 失败（%v），回落 %s", a.UID, endpointV3Config, err, endpointConsoleModels)
		out, err = c.fetchModelsFrom(a, endpointConsoleModels)
		if err != nil {
			return nil, err
		}
	}
	// 合并进 effort 能力缓存（供请求体降级；无 supportedEfforts 的模型不入缓存）。
	// 合并而非整表替换：两个域各取一次时，后取的不能把先取的域清掉。
	c.effortsMu.Lock()
	if c.efforts == nil {
		c.efforts = make(map[string][]string, len(out))
	}
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			c.efforts[mi.ID] = mi.Efforts
		}
	}
	c.effortsMu.Unlock()
	return out, nil
}

// fetchModelsFrom 请求一个目录端点并解析。两个端点的 data 同形：models 全集 + agents。
func (c *Client) fetchModelsFrom(a *auth.Auth, path string) ([]ModelInfo, error) {
	req, err := http.NewRequest(http.MethodGet, c.chatBase(a)+path, nil)
	if err != nil {
		return nil, err
	}
	if path == endpointV3Config {
		c.CommonHeaders(req, a)
	} else {
		origin := c.originRefererFor(a)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		req.Header.Set("User-Agent", c.userAgent())
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	return parseModelCatalog(raw)
}

// parseModelCatalog 解析目录响应：只保留 agents 里 cli 的模型列表（按其顺序），
// 跳过 disabled 与目录里查不到的 id。data.models 是全集（实测国内版 32 个里只有 17 个
// 在 cli 列表，其余是补全/图片/内部模型），必须按 cli 列表过滤。
func parseModelCatalog(raw []byte) ([]ModelInfo, error) {
	type agent struct {
		Name   string   `json:"name"`
		Models []string `json:"models"`
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID                string   `json:"id"`
				Name              string   `json:"name"`
				DescriptionZh     string   `json:"descriptionZh"`
				DescriptionEn     string   `json:"descriptionEn"`
				Credits           string   `json:"credits"`
				Vendor            string   `json:"vendor"`
				Tags              []string `json:"tags"`
				IsDefault         bool     `json:"isDefault"`
				OnlyReasoning     bool     `json:"onlyReasoning"`
				SupportsImages    bool     `json:"supportsImages"`
				SupportsReasoning bool     `json:"supportsReasoning"`
				SupportsToolCall  bool     `json:"supportsToolCall"`
				MaxAllowedSize    int64    `json:"maxAllowedSize"`
				MaxInputTokens    int64    `json:"maxInputTokens"`
				MaxOutputTokens   int64    `json:"maxOutputTokens"`
				Disabled          bool     `json:"disabled"`
				Reasoning         struct {
					Effort           string   `json:"effort"`
					DefaultEffort    string   `json:"defaultEffort"`
					SupportedEfforts []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
			Agents []agent `json:"agents"`
			// 未登录时 /v3/config 给的是 data.agent.agents 形态，一并兼容。
			Agent struct {
				Agents []agent `json:"agents"`
			} `json:"agent"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range append(env.Data.Agents, env.Data.Agent.Agents...) {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	byID := make(map[string]int, len(env.Data.Models))
	for i, m := range env.Data.Models {
		byID[m.ID] = i
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		i, ok := byID[id]
		if !ok || env.Data.Models[i].Disabled {
			continue
		}
		m := env.Data.Models[i]
		desc := m.DescriptionZh
		if desc == "" {
			desc = m.DescriptionEn
		}
		effort := m.Reasoning.DefaultEffort
		if effort == "" {
			effort = m.Reasoning.Effort
		}
		out = append(out, ModelInfo{
			ID: m.ID, Name: m.Name, ContextWindow: m.MaxInputTokens, MaxTokens: m.MaxOutputTokens,
			Efforts: m.Reasoning.SupportedEfforts, Description: desc, Credits: m.Credits, Vendor: m.Vendor,
			Tags: m.Tags, IsDefault: m.IsDefault, OnlyReasoning: m.OnlyReasoning, SupportsImages: m.SupportsImages,
			SupportsReasoning: m.SupportsReasoning, SupportsToolCall: m.SupportsToolCall,
			MaxAllowedSize: m.MaxAllowedSize, ReasoningEffort: effort,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// IsGlobalRealm 报告账号是否属于国际版（workbuddy.ai）；其余（含空 domain）为国内版。
func IsGlobalRealm(a *auth.Auth) bool { return isGlobalRealm(a) }

// UserResource 查询账号当前可花费积分余额（所有套餐 CycleCapacity 聚合，负值钳 0）。
func (c *Client) UserResource(a *auth.Auth) (remain int64, err error) {
	url := c.billingBase(a) + "/v2/billing/meter/get-user-resource"
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	c.BillingHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		var r int64
		switch {
		case acct.CycleCapacitySize > 0:
			r = acct.CycleCapacityRemain
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			r = acct.CycleCapacityRemain
		default:
			r = acct.CapacityRemain
		}
		if r < 0 {
			r = 0
		}
		remain += r
	}
	return remain, nil
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	url := c.billingBase(a) + "/v2/billing/meter/daily-checkin"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	c.BillingHeaders(req, a)
	_, err = c.doJSON(req)
	return err
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
