package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func doReq(h *Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestAccountPauseResume 停用后 /status 透出 paused、固定号端点拒绝；启用后恢复。
func TestAccountPauseResume(t *testing.T) {
	const uid = "u-pause"
	h := pinnedTestHandler(t, uid)

	if w := doReq(h, http.MethodPost, "/v1/accounts/"+uid+"/pause", ""); w.Code != http.StatusOK {
		t.Fatalf("pause code=%d body=%s", w.Code, w.Body)
	}
	var st struct {
		Paused   int `json:"paused"`
		Healthy  int `json:"healthy"`
		Accounts []struct {
			UID    string `json:"uid"`
			Paused bool   `json:"paused"`
		} `json:"accounts"`
	}
	w := doReq(h, http.MethodGet, "/status", "")
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Paused != 1 || st.Healthy != 0 || len(st.Accounts) != 1 || !st.Accounts[0].Paused {
		t.Errorf("/status 未反映停用: %s", w.Body)
	}

	w = doReq(h, http.MethodPost, "/v1/a/"+uid+"/chat/completions",
		`{"model":"glm-5.1","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "unavailable") {
		t.Errorf("停用账号的固定号请求应 503 unavailable, got %d %s", w.Code, w.Body)
	}

	if w := doReq(h, http.MethodPost, "/v1/accounts/"+uid+"/resume", ""); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"paused":false`) {
		t.Fatalf("resume code=%d body=%s", w.Code, w.Body)
	}
	if st, _ := h.cfg.Pool.Status(uid); st.Paused {
		t.Error("resume 后不应再停用")
	}
}

// TestAccountPauseUnknown 未知账号 404。
func TestAccountPauseUnknown(t *testing.T) {
	h := pinnedTestHandler(t, "u1")
	if w := doReq(h, http.MethodPost, "/v1/accounts/nope/pause", ""); w.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", w.Code)
	}
}
