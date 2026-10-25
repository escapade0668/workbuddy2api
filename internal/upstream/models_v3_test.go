package upstream

import (
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
)

type v3RoundTrip func(*http.Request) (*http.Response, error)

func (f v3RoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// v3Client 按请求路径返回不同响应，并记录请求过的 URL。
func v3Client(t *testing.T, byPath map[string]func() (int, string)) (*Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	c := New()
	c.ChatBaseCN = "https://cn.fake"
	c.ChatBaseGlobal = "https://global.fake"
	c.HTTP = &http.Client{Transport: v3RoundTrip(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		seen = append(seen, r.URL.Host+r.URL.Path+" UA="+r.Header.Get("User-Agent"))
		mu.Unlock()
		status, body := 404, "not found"
		if fn, ok := byPath[r.URL.Path]; ok {
			status, body = fn()
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	return c, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

var globalAcct = &auth.Auth{UID: "g", AccessToken: "tok", Domain: "www.workbuddy.ai"}
var cnAcct = &auth.Auth{UID: "c", AccessToken: "tok", Domain: "www.codebuddy.cn"}

// testdata 是真实账号用网关同款 CLI 头取到的 /v3/config（裁剪）。2026-10-05 实测：
//   - /console/enterprises/personal/models 在国际版恒 500（HTML），国际版只能从 /v3/config 取；
//   - 必须用网关自己的 CLI 头取：桌面端 UA 拿到的列表里有 CLI 身份调不通的模型
//     （如 gpt-6-luna → "service info not found"），CLI UA 列表里的都实测可调。
func TestFetchModelsFromV3ConfigGlobal(t *testing.T) {
	c, seen := v3Client(t, map[string]func() (int, string){
		"/v3/config": func() (int, string) { return 200, readFixture(t, "v3config-cli-global.json") },
	})
	infos, err := c.FetchModels(globalAcct)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range infos {
		ids = append(ids, m.ID)
	}
	// 只保留 cli 列表（按其顺序），剔除列表外的 deepseek-v4.1-flash-sg。
	if got := strings.Join(ids, ","); got != "default-model,deep-model,hy4-preview,gpt-5.6-luna,glm-5.2" {
		t.Fatalf("ids = %s", got)
	}
	reqs := seen()
	if len(reqs) != 1 || !strings.HasPrefix(reqs[0], "global.fake/v3/config UA="+(&Client{}).userAgent()) {
		t.Fatalf("应只用 CLI 头请求国际版 /v3/config: %v", reqs)
	}
	glm := infos[4]
	if glm.Credits != "x0.79" || glm.ContextWindow != 1000000 || glm.MaxTokens != 48000 || glm.Name != "GLM-5.2" {
		t.Errorf("字段映射不对: %+v", glm)
	}
	if len(glm.Efforts) == 0 || !glm.SupportsToolCall || glm.Description == "" {
		t.Errorf("efforts/能力/描述缺失: %+v", glm)
	}
	if infos[0].Credits != "" { // default-model 上游 credits 为空串：保持为空，不当免费
		t.Errorf("default-model credits = %q", infos[0].Credits)
	}
}

func TestFetchModelsFromV3ConfigCN(t *testing.T) {
	c, _ := v3Client(t, map[string]func() (int, string){
		"/v3/config": func() (int, string) { return 200, readFixture(t, "v3config-cli-cn.json") },
	})
	infos, err := c.FetchModels(cnAcct)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 || infos[2].ID != "minimax-m2.7" || infos[2].Credits != "x0.19 credits" {
		t.Fatalf("infos = %+v", infos)
	}
}

// /v3/config 失败时回落旧端点（国内版旧端点可用），不至于直接掉回静态表。
func TestFetchModelsFallsBackToConsoleEndpoint(t *testing.T) {
	c, seen := v3Client(t, map[string]func() (int, string){
		"/v3/config": func() (int, string) { return 500, "<html>500</html>" },
		"/console/enterprises/personal/models": func() (int, string) {
			return 200, `{"code":0,"data":{"models":[{"id":"glm-5.2","maxInputTokens":131072}],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`
		},
	})
	infos, err := c.FetchModels(cnAcct)
	if err != nil || len(infos) != 1 || infos[0].ID != "glm-5.2" {
		t.Fatalf("infos=%+v err=%v seen=%v", infos, err, seen())
	}
}

// 两个域各取一次后，effort 能力缓存应合并而不是后者覆盖前者。
func TestFetchModelsMergesEffortsAcrossRealms(t *testing.T) {
	c, _ := v3Client(t, map[string]func() (int, string){
		"/v3/config": func() (int, string) { return 200, readFixture(t, "v3config-cli-global.json") },
	})
	if _, err := c.FetchModels(globalAcct); err != nil {
		t.Fatal(err)
	}
	c2, _ := v3Client(t, map[string]func() (int, string){
		"/v3/config": func() (int, string) { return 200, readFixture(t, "v3config-cli-cn.json") },
	})
	c.HTTP = c2.HTTP
	if _, err := c.FetchModels(cnAcct); err != nil {
		t.Fatal(err)
	}
	eff := c.effortsSnapshot()
	// 国内版这次没给任何 supportedEfforts；旧实现整表替换，会把国际版取到的清空。
	if len(eff["hy4-preview"]) == 0 || len(eff["glm-5.2"]) == 0 {
		t.Fatalf("国际版取到的 efforts 被国内版那次覆盖了: %v", eff)
	}
}
