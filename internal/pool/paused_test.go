package pool

import (
	"path/filepath"
	"time"

	"testing"
	"workbuddy2api/internal/auth"
)

// TestSetPausedExcludesFromPick 人工停用的账号退出所有选号路径（普通/固定号/全冷却兜底），
// 启用后立即回到池子。
func TestSetPausedExcludesFromPick(t *testing.T) {
	p := newPoolWith("u1")
	if !p.SetPaused("u1", true) {
		t.Fatal("SetPaused 对已存在账号应返回 true")
	}
	if a := p.Pick(); a != nil {
		t.Errorf("停用账号不应被 Pick 选中: %s", a.UID)
	}
	if a := p.PickByUID("u1"); a != nil {
		t.Error("停用账号不应被 PickByUID（固定号/粘性）选中")
	}
	if a := p.PickExcludingForModel(nil, "glm-5.1"); a != nil {
		t.Error("停用账号不应被模型感知选号选中")
	}
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Errorf("停用账号不应出现在 AvailableUIDs: %v", uids)
	}
	if p.ServableNow() {
		t.Error("唯一账号停用后池不可服务")
	}

	p.SetPaused("u1", false)
	if a := p.Pick(); a == nil || a.UID != "u1" {
		t.Error("启用后账号应可被选中")
	}
}

// TestPausedSkippedByFallback 全冷却兜底不得选中停用账号（即便它冷却最早到期）。
func TestPausedSkippedByFallback(t *testing.T) {
	p := newPoolWith("u1")
	p.Cooldown("u1", CoolSoft, time.Minute, "429")
	p.SetPaused("u1", true)
	if a := p.Pick(); a != nil {
		t.Errorf("停用账号不应参与全冷却兜底: %s", a.UID)
	}
}

// TestSetPausedUnknownUID 未知账号返回 false。
func TestSetPausedUnknownUID(t *testing.T) {
	p := newPoolWith("u1")
	if p.SetPaused("nope", true) {
		t.Error("未知账号应返回 false")
	}
}

// TestPausedStatusAndCounts 状态透出 paused；计数里停用账号既不算 healthy 也不算 cooling。
func TestPausedStatusAndCounts(t *testing.T) {
	p := newPoolWith("u1", "u2", "u3")
	p.SetPaused("u1", true)
	p.SetPaused("u3", true)
	p.Disable("u3", "x") // 系统禁用优先：不重复计入 paused

	st, _ := p.Status("u1")
	if !st.Paused || st.Disabled {
		t.Errorf("u1 status=%+v，期望 paused 且非 disabled", st)
	}
	total, healthy, cooling, disabled, _ := p.CountsDetailed()
	if total != 3 || healthy != 1 || cooling != 0 || disabled != 1 {
		t.Errorf("counts total=%d healthy=%d cooling=%d disabled=%d", total, healthy, cooling, disabled)
	}
	if n := p.PausedCount(); n != 1 {
		t.Errorf("PausedCount=%d want 1", n)
	}
}

// TestPausedSurvivesSigninReenable 签到解冻（ReenableIfCredits）只清冷却，不得顺手启用停用账号。
func TestPausedSurvivesSigninReenable(t *testing.T) {
	p := newPoolWith("u1")
	p.SetPaused("u1", true)
	p.ReenableIfCredits("u1", 500)
	st, _ := p.Status("u1")
	if !st.Paused {
		t.Error("签到解冻不应清除人工停用")
	}
	if st.Credits != 500 {
		t.Errorf("停用账号签到后仍应更新积分, credits=%d", st.Credits)
	}
}

// TestPausedPersisted 停用状态随 state.json 持久化，重启后保持。
func TestPausedPersisted(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.SyncToDir(newPoolWith("u1").authsForTest())
	p.SetPaused("u1", true)
	p.Flush()

	p2 := New(fp)
	st, ok := p2.Status("u1")
	if !ok || !st.Paused {
		t.Errorf("重启后应保持停用: ok=%v st=%+v", ok, st)
	}
}

// authsForTest 取出池内全部凭证（测试辅助）。
func (p *Pool) authsForTest() []*auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*auth.Auth, 0, len(p.byUID))
	for _, e := range p.byUID {
		out = append(out, e.a)
	}
	return out
}
