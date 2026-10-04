package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// 两个域各一个账号。uid 排序上 c1 < g1：在等权、随机源恒为 0 的测试池里，
// 不做域偏好时总是先选到国内版 c1 —— 用来证明偏好确实生效。
var (
	cnAuth     = &auth.Auth{UID: "c1", AccessToken: "at-cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999}
	globalAuth = &auth.Auth{UID: "g1", AccessToken: "at-global", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999}
)

func resetModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.byRealm = nil
	dynamicModelsCache.Unlock()
}

func upstreamFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../upstream/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// realmFake 按 host 区分两个域：/v3/config 返回各域目录，聊天请求交给 chat 回调。
type realmFake struct {
	mu        sync.Mutex
	configs   map[string]func() (int, string) // host → /v3/config 响应
	chat      func(authz string) (int, string)
	chatAuths []string
}

func (f *realmFake) client() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			status, body, ct := 404, "not found", "application/json"
			if strings.HasSuffix(r.URL.Path, "/chat/completions") {
				f.mu.Lock()
				f.chatAuths = append(f.chatAuths, r.Header.Get("Authorization"))
				f.mu.Unlock()
				status, body = f.chat(r.Header.Get("Authorization"))
				if status == 200 {
					ct = "text/event-stream"
				}
			} else if fn, ok := f.configs[r.URL.Host]; ok && r.URL.Path == "/v3/config" {
				status, body = fn()
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{ct}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN:     "https://cn.fake",
		ChatBaseGlobal: "https://global.fake",
	}
}

func (f *realmFake) auths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.chatAuths...)
}

func bothRealmsFake(t *testing.T) *realmFake {
	return &realmFake{
		configs: map[string]func() (int, string){
			"cn.fake":     func() (int, string) { return 200, upstreamFixture(t, "v3config-cli-cn.json") },
			"global.fake": func() (int, string) { return 200, upstreamFixture(t, "v3config-cli-global.json") },
		},
		chat: func(string) (int, string) { return 200, sseOK },
	}
}

func getModels(t *testing.T, h *Handler) map[string]map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	out := map[string]map[string]any{}
	for _, m := range resp.Data {
		out[m["id"].(string)] = m
	}
	return out
}

func realmsOf(m map[string]any) string {
	var rs []string
	for _, r := range m["realms"].([]any) {
		rs = append(rs, r.(string))
	}
	return strings.Join(rs, ",")
}

// 双域账号：/v1/models 应是两个域目录的并集，并标出每个模型可用的域。
// 旧实现只用随机一个账号取目录（国际版还恒 500），另一个域的模型整个缺失。
func TestModelsUnionAcrossRealms(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	f := bothRealmsFake(t)
	h := NewHandler(Config{Pool: testPoolWith(cnAuth, globalAuth), Upstream: f.client()})
	models := getModels(t, h)

	if len(models) != 7 { // 国际 5 + 国内 3，glm-5.2 两域共有
		t.Fatalf("want 7 models, got %d: %v", len(models), models)
	}
	cases := map[string]string{"glm-5.2": "cn,global", "hy4-preview": "global", "minimax-m2.7": "cn", "deep-model": "global"}
	for id, want := range cases {
		if got := realmsOf(models[id]); got != want {
			t.Errorf("%s realms = %s, want %s", id, got, want)
		}
	}
	// 单域模型直接给倍率；两个域倍率原文不同的给 credits_by_realm，不擅自挑一个。
	if models["hy4-preview"]["credits"] != "x0.29" {
		t.Errorf("hy4-preview credits = %v", models["hy4-preview"]["credits"])
	}
	byRealm, _ := models["glm-5.2"]["credits_by_realm"].(map[string]any)
	if byRealm["cn"] != "x0.79 credits" || byRealm["global"] != "x0.79" {
		t.Errorf("glm-5.2 credits_by_realm = %v", models["glm-5.2"]["credits_by_realm"])
	}
	if models["glm-5.2"]["context_length"].(float64) != 1000000 {
		t.Errorf("glm-5.2 context_length = %v", models["glm-5.2"]["context_length"])
	}
}

// 目录拉取失败不是账号的错：不能记账号错误/熔断（旧实现会 NoteError，
// 国际版账号因此每次被选中都白挨一次罚）。
func TestModelsFetchFailureDoesNotPenalizeAccount(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	f := &realmFake{configs: map[string]func() (int, string){
		"global.fake": func() (int, string) { return 500, "<html>500</html>" },
	}}
	p := testPoolWith(globalAuth)
	h := NewHandler(Config{Pool: p, Upstream: f.client()})
	models := getModels(t, h)
	if _, ok := models["glm-5.2"]; !ok || len(models) < 5 {
		t.Fatalf("全部失败时应回落静态表: %v", models)
	}
	st, _ := p.Status(globalAuth.UID)
	if st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Fatalf("目录失败不应计入账号错误: %+v", st)
	}
}

func chat(t *testing.T, h *Handler, model string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

// 已知只在国际版可用的模型，应优先派给国际版账号（否则随机落到国内版必败、白白轮转）。
func TestChatPrefersAccountsInModelRealm(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	f := bothRealmsFake(t)
	h := NewHandler(Config{Pool: testPoolWith(cnAuth, globalAuth), Upstream: f.client()})
	getModels(t, h) // 预热目录

	if rec := chat(t, h, "hy4-preview"); rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := f.auths(); len(got) != 1 || got[0] != "Bearer at-global" {
		t.Fatalf("国际版独有模型应直接派给国际版账号，实际请求: %v", got)
	}
	if rec := chat(t, h, "minimax-m2.7"); rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if got := f.auths(); got[len(got)-1] != "Bearer at-cn" {
		t.Fatalf("国内版独有模型应派给国内版账号，实际请求: %v", got)
	}
}

// 目录只是偏好不是硬过滤：偏好的域没有可用账号时照常落到其他域（目录可能不全）。
func TestChatFallsBackWhenPreferredRealmUnavailable(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	f := bothRealmsFake(t)
	p := testPoolWith(cnAuth, globalAuth)
	h := NewHandler(Config{Pool: p, Upstream: f.client()})
	getModels(t, h)
	p.Disable(globalAuth.UID, "test")

	if rec := chat(t, h, "hy4-preview"); rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := f.auths(); len(got) != 1 || got[0] != "Bearer at-cn" {
		t.Fatalf("应回落到国内版账号: %v", got)
	}
}

// 目录里没有的模型不受影响（实测有些不在 cli 列表的模型照样能调）。
func TestChatUnknownModelNotRestricted(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	f := bothRealmsFake(t)
	h := NewHandler(Config{Pool: testPoolWith(cnAuth, globalAuth), Upstream: f.client()})
	getModels(t, h)
	if rec := chat(t, h, "auto"); rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
}

// 所有账号都被上游以「客户端错误」拒绝（如 11102 模型无权限）时，应把上游的 4xx 与原因
// 交给客户端，而不是报 503 no_healthy_account —— 账号池是健康的，是这个模型用不了。
func TestChatClientErrorPassesThroughUpstreamStatus(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	f := bothRealmsFake(t)
	f.chat = func(string) (int, string) {
		return 400, `{"code":11102,"msg":"model [gpt-5.6-luna] is only available for authorized users"}`
	}
	p := testPoolWith(cnAuth, globalAuth)
	h := NewHandler(Config{Pool: p, Upstream: f.client()})
	rec := chat(t, h, "gpt-5.6-luna")
	if rec.Code != 400 {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "only available for authorized users") || strings.Contains(rec.Body.String(), "no_healthy_account") {
		t.Fatalf("应透出上游原因: %s", rec.Body)
	}
	for _, uid := range []string{cnAuth.UID, globalAuth.UID} {
		if st, _ := p.Status(uid); st.Cooling || st.Disabled || st.BreakerFails != 0 {
			t.Fatalf("客户端错误不应罚账号: %+v", st)
		}
	}
}

// 真正的账号池故障（如 5xx）仍报 503。
func TestChatServerErrorsStill503(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	f := bothRealmsFake(t)
	f.chat = func(string) (int, string) { return 500, `boom` }
	h := NewHandler(Config{Pool: testPoolWith(cnAuth, globalAuth), Upstream: f.client()})
	if rec := chat(t, h, "glm-5.2"); rec.Code != 503 {
		t.Fatalf("want 503, got %d", rec.Code)
	}
}

// 固定号路由（/v1/a/<uid>，供 9Router 等按账号连接轮转）保持 503：
// 上层路由器靠 503 切到下一个账号连接，而别的账号可能恰好有该模型的权限。
func TestPinnedChatClientErrorStays503(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	f := bothRealmsFake(t)
	f.chat = func(string) (int, string) {
		return 400, `{"code":11102,"msg":"model [gpt-5.6-luna] is only available for authorized users"}`
	}
	h := NewHandler(Config{Pool: testPoolWith(cnAuth, globalAuth), Upstream: f.client()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/a/g1/chat/completions",
		strings.NewReader(`{"model":"gpt-5.6-luna","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "only available for authorized users") {
		t.Fatalf("固定号应保持 503 并带上游原因: %d %s", rec.Code, rec.Body)
	}
	// 固定号只打指定账号：失败不得轮转到别的账号（此前会静默换到国内版账号，
	// 调用方以为是国际版账号在答，统计也记到别的号上）。
	if got := f.auths(); len(got) != 1 || got[0] != "Bearer at-global" {
		t.Fatalf("固定号请求不应落到其他账号: %v", got)
	}
}

// 固定号账号当前不可用（禁用/冷却/在途满）时直接报错，不改派给池里其他账号。
func TestPinnedChatUnavailableAccountDoesNotFallBack(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	f := bothRealmsFake(t)
	p := testPoolWith(cnAuth, globalAuth)
	p.Disable(globalAuth.UID, "test")
	h := NewHandler(Config{Pool: p, Upstream: f.client()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/a/g1/chat/completions",
		strings.NewReader(`{"model":"hy3","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 503 {
		t.Fatalf("want 503, got %d %s", rec.Code, rec.Body)
	}
	if got := f.auths(); len(got) != 0 {
		t.Fatalf("不应改派给其他账号: %v", got)
	}
}
