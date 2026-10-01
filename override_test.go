package automation

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func mustAddRule(t *testing.T, core *Core, rule Rule) {
	t.Helper()
	if err := core.AddRule(rule); err != nil {
		t.Fatal(err)
	}
}

func mustProcess(t *testing.T, core *Core, event Event) {
	t.Helper()
	if _, err := core.Process(event); err != nil {
		t.Fatal(err)
	}
}

func mustTick(t *testing.T, core *Core, at time.Time) {
	t.Helper()
	if _, err := core.Tick(at); err != nil {
		t.Fatal(err)
	}
}

func decisionsWithReason(decisions []Decision, reason Reason) []Decision {
	var matched []Decision
	for _, decision := range decisions {
		if decision.Reason == reason {
			matched = append(matched, decision)
		}
	}
	return matched
}

// 房间接管与设备临时例外两层并存：撤销房间层后，设备例外层仍然有效，
// 其余设备恢复自动控制，而不是一律回到最早的自动规则。
func TestLayeredOverridesRoomAndDeviceException(t *testing.T) {
	now := time.Unix(5000, 0)
	core, err := NewCore(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sensor := VirtualDevice{ID: "daylight"}
	lamp := VirtualDevice{ID: "lamp", Room: "living"}
	ac := VirtualDevice{ID: "ac", Room: "living"}
	mustAddRule(t, core, Rule{
		ID:        "lamp-on",
		Condition: DeviceProperty{DeviceID: "daylight", Name: "dark", Operator: OpEq, Value: 1},
		Duration:  time.Minute,
		Action:    SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 1},
	})
	mustAddRule(t, core, Rule{
		ID:        "ac-on",
		Condition: DeviceProperty{DeviceID: "daylight", Name: "dark", Operator: OpEq, Value: 1},
		Duration:  time.Minute,
		Action:    SetStateAction{DeviceID: "ac", Name: PoweredOn, Value: 1},
	})

	// 上报房间归属，随后两条规则各触发一次。
	mustProcess(t, core, lamp.Event(PoweredOn, 0, now))
	mustProcess(t, core, ac.Event(PoweredOn, 0, now))
	mustProcess(t, core, sensor.Event("dark", 1, now))
	mustTick(t, core, now.Add(time.Minute))
	if got := len(core.AppliedActions()); got != 2 {
		t.Fatalf("expected both rules fired, got %d", got)
	}

	// 第一层：整个 living 房间人工接管（无限期）。
	roomLayer := Override{ID: "room-living", Scope: OverrideScope{Room: "living"}, Reason: "party"}
	if err := core.Takeover(roomLayer, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// 重新装弹条件：覆盖期内任何规则都不得触发。
	mustProcess(t, core, sensor.Event("dark", 0, now.Add(3*time.Minute)))
	mustProcess(t, core, sensor.Event("dark", 1, now.Add(4*time.Minute)))
	mustTick(t, core, now.Add(5*time.Minute))
	if got := len(core.AppliedActions()); got != 2 {
		t.Fatalf("room override must block every rule in scope, got %d applied", got)
	}

	// 第二层：lamp 的临时例外，独立到期时刻。
	lampExpiry := now.Add(36 * time.Minute)
	exception := Override{ID: "lamp-exception", Scope: OverrideScope{DeviceID: "lamp"}, ExpiresAt: &lampExpiry}
	if err := core.Takeover(exception, now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}

	// 撤销房间层：ac 不再被任何层覆盖，重新观察条件后触发；
	// lamp 仍被设备例外层罩住，保持人工控制。
	if err := core.RevokeOverride("room-living", now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	mustTick(t, core, now.Add(11*time.Minute))
	mustTick(t, core, now.Add(11*time.Minute+time.Second))
	applied := core.AppliedActions()
	if got := len(applied); got != 3 {
		t.Fatalf("only ac rule should refire after room layer revoked, got %d", got)
	}
	if applied[2].Action.DeviceID != "ac" {
		t.Fatalf("expected ac to resume automation, got %+v", applied[2].Action)
	}
	if len(core.ActiveOverrides()) != 1 || core.ActiveOverrides()[0].ID != "lamp-exception" {
		t.Fatalf("device exception layer must survive room revocation: %+v", core.ActiveOverrides())
	}

	// 设备例外层到期后 lamp 才恢复自动：重新持续 1 分钟再触发。
	mustTick(t, core, lampExpiry)
	mustTick(t, core, lampExpiry.Add(time.Minute+time.Second))
	applied = core.AppliedActions()
	if got := len(applied); got != 4 || applied[3].Action.DeviceID != "lamp" {
		t.Fatalf("lamp should resume only after its own layer expired: %+v", applied)
	}

	decisions := core.Decisions()
	if got := len(decisionsWithReason(decisions, ReasonOverrideStarted)); got != 2 {
		t.Fatalf("expected two override_started decisions, got %d", got)
	}
	revoked := decisionsWithReason(decisions, ReasonOverrideRevoked)
	if len(revoked) != 1 || revoked[0].OverrideID != "room-living" {
		t.Fatalf("missing room-living revocation decision: %+v", revoked)
	}
	expired := decisionsWithReason(decisions, ReasonOverrideExpired)
	if len(expired) != 1 || expired[0].OverrideID != "lamp-exception" || !expired[0].At.Equal(lampExpiry) {
		t.Fatalf("expiry decision must carry the layer and its due moment: %+v", expired)
	}
}

// 重启跨越接管到期时刻：到期决策记录本应发生的时刻；过时的延迟动作
// 不补发，而是以 stale_action_expired 取消并说明原到点时刻。
func TestRestartAcrossOverrideExpiryAndStaleAction(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	now := time.Unix(6000, 0)
	store := FileStore{Path: statePath}
	core, err := NewCore(store, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sensor := VirtualDevice{ID: "daylight"}
	rule := Rule{
		ID:        "night-light",
		Condition: DeviceProperty{DeviceID: "daylight", Name: "dark", Operator: OpEq, Value: 1},
		Duration:  time.Minute,
		Action:    SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 1, After: 2 * time.Minute, ValidFor: 5 * time.Minute},
	}
	mustAddRule(t, core, rule)
	mustProcess(t, core, sensor.Event("dark", 1, now))
	mustTick(t, core, now.Add(time.Minute))
	if got := len(core.PendingActions()); got != 1 {
		t.Fatalf("expected pending action, got %d", got)
	}

	// 设备层接管，到期时刻落在即将到来的停机窗口内。
	expiry := now.Add(5 * time.Minute)
	if err := core.Takeover(Override{ID: "lamp-hold", Scope: OverrideScope{DeviceID: "lamp"}, ExpiresAt: &expiry}, now.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := len(core.PendingActions()); got != 0 {
		t.Fatalf("takeover must cancel covered pending actions, got %d", got)
	}

	// 另排一个会过时的新动作不太现实（接管期内规则不触发），
	// 因此直接验证：停机前由第二条无接管路径产生的动作在停机后过时取消。
	// 这里改用纯过时场景：单独核验证 ValidFor。
	staleCore, err := NewCore(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	mustAddRule(t, staleCore, rule)
	mustProcess(t, staleCore, sensor.Event("dark", 1, now))
	mustTick(t, staleCore, now.Add(time.Minute))
	// 停机到远超 ValidFor 之后：动作不得补发。
	mustTick(t, staleCore, now.Add(20*time.Minute))
	if _, ok := staleCore.Snapshot().Value("lamp", PoweredOn); ok {
		t.Fatal("stale action was reissued after downtime")
	}
	stale := decisionsWithReason(staleCore.Decisions(), ReasonStaleAction)
	if len(stale) != 1 || stale[0].Detail == "" {
		t.Fatalf("expected stale_action_expired decision with due-time detail: %+v", stale)
	}

	// 重启：跨越 lamp-hold 的到期时刻 t+5m，在 t+10m 恢复。
	restarted, err := NewCore(store, func() time.Time { return now.Add(10 * time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	mustAddRule(t, restarted, rule)
	mustTick(t, restarted, now.Add(10*time.Minute))
	if got := len(restarted.ActiveOverrides()); got != 0 {
		t.Fatalf("override should have expired during downtime, still active: %+v", restarted.ActiveOverrides())
	}
	expired := decisionsWithReason(restarted.Decisions(), ReasonOverrideExpired)
	if len(expired) != 1 || !expired[0].At.Equal(expiry) {
		t.Fatalf("expiry decision must record the moment it should have happened: %+v", expired)
	}
	// 接管层在停机期间到期：恢复后规则重新观察条件，持续 1 分钟后才动作。
	mustTick(t, restarted, now.Add(11*time.Minute))
	mustTick(t, restarted, now.Add(13*time.Minute+time.Second))
	if value, _ := restarted.Snapshot().Value("lamp", PoweredOn); value != 1 {
		t.Fatalf("lamp should turn on only after fresh duration post-restart: %v", value)
	}
}

// 规则竞争、设备离线与人工接管交错：决策日志能逐步解释最终状态。
func TestCompetitionOfflineAndOverrideExplainable(t *testing.T) {
	now := time.Unix(7000, 0)
	core, err := NewCore(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sensor := VirtualDevice{ID: "daylight"}
	lamp := VirtualDevice{ID: "lamp"}
	mustAddRule(t, core, Rule{
		ID:        "night-on",
		Priority:  1,
		Condition: DeviceProperty{DeviceID: "daylight", Name: "dark", Operator: OpEq, Value: 1},
		Action:    SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 1, After: time.Minute},
	})
	mustAddRule(t, core, Rule{
		ID:        "away-off",
		Priority:  10,
		Condition: DeviceProperty{DeviceID: "mode", Name: "away", Operator: OpEq, Value: 1},
		Action:    SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 0, After: time.Minute},
	})

	mustProcess(t, core, sensor.Event("dark", 1, now))
	mustProcess(t, core, (&VirtualDevice{ID: "mode"}).Event("away", 1, now.Add(10*time.Second)))
	// 高优先级 away-off 在排定时即裁决掉 night-on。
	if got := decisionsWithReason(core.Decisions(), ReasonSuperseded); len(got) != 1 || got[0].RuleID != "night-on" {
		t.Fatalf("night-on should lose arbitration: %+v", core.Decisions())
	}
	// 落地前设备离线：赢家的动作到点也必须取消，不得下发。
	mustProcess(t, core, lamp.Availability(false, now.Add(20*time.Second)))
	mustTick(t, core, now.Add(2*time.Minute))
	if _, ok := core.Snapshot().Value("lamp", PoweredOn); ok {
		t.Fatal("action delivered to offline device")
	}
	offline := decisionsWithReason(core.Decisions(), ReasonDeviceOffline)
	if len(offline) != 1 || offline[0].RuleID != "away-off" {
		t.Fatalf("expected device_offline cancellation for away-off: %+v", offline)
	}
	// 离线期间人工接管该设备；设备恢复在线后接管层依然有效。
	if err := core.Takeover(Override{ID: "lamp-manual", Scope: OverrideScope{DeviceID: "lamp"}}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	mustProcess(t, core, lamp.Availability(true, now.Add(4*time.Minute)))
	mustProcess(t, core, sensor.Event("dark", 0, now.Add(5*time.Minute)))
	mustProcess(t, core, sensor.Event("dark", 1, now.Add(6*time.Minute)))
	mustTick(t, core, now.Add(7*time.Minute))
	if _, ok := core.Snapshot().Value("lamp", PoweredOn); ok {
		t.Fatal("override layer must keep blocking after device reconnects")
	}
	// 撤销接管层后恢复的是“当前仍有效的控制关系”：被重新装弹的规则里
	// 高优先级的 away-off 仍然成立并赢得裁决，而不是回到最早的开灯规则。
	if err := core.RevokeOverride("lamp-manual", now.Add(8*time.Minute)); err != nil {
		t.Fatal(err)
	}
	mustTick(t, core, now.Add(9*time.Minute))
	mustTick(t, core, now.Add(9*time.Minute+time.Second))
	if value, ok := core.Snapshot().Value("lamp", PoweredOn); !ok || value != 0 {
		t.Fatalf("higher-priority away-off should win after override revoked: %v", value)
	}
	var lastExecuted *Decision
	for i, decision := range core.Decisions() {
		if decision.Outcome == OutcomeExecuted {
			lastExecuted = &core.Decisions()[i]
		}
	}
	if lastExecuted == nil || lastExecuted.RuleID != "away-off" {
		t.Fatalf("final executed action must be explainable as away-off: %+v", core.Decisions())
	}
}

// 虚拟时间回放：含接管层建立/撤销的历史重放出同一裁决序列。
func TestReplayReproducesOverrideDecisions(t *testing.T) {
	now := time.Unix(8000, 0)
	core, err := NewCore(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sensor := VirtualDevice{ID: "daylight"}
	rule := Rule{
		ID:        "night-light",
		Condition: DeviceProperty{DeviceID: "daylight", Name: "dark", Operator: OpEq, Value: 1},
		Duration:  time.Minute,
		Action:    SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 1, After: time.Minute},
	}
	mustAddRule(t, core, rule)
	mustProcess(t, core, sensor.Event("dark", 1, now))
	mustTick(t, core, now.Add(time.Minute))
	if err := core.Takeover(Override{ID: "hold", Scope: OverrideScope{DeviceID: "lamp"}}, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	mustProcess(t, core, sensor.Event("dark", 0, now.Add(3*time.Minute)))
	mustProcess(t, core, sensor.Event("dark", 1, now.Add(4*time.Minute)))
	if err := core.RevokeOverride("hold", now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	mustTick(t, core, now.Add(6*time.Minute))
	mustTick(t, core, now.Add(7*time.Minute+time.Second))

	replayed, decisions, err := Replay(nil, []Rule{rule}, core.History(), now.Add(7*time.Minute+time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decisions, core.Decisions()) {
		t.Fatalf("replay diverged: live=%+v replay=%+v", core.Decisions(), decisions)
	}
	if value, _ := replayed.Snapshot().Value("lamp", PoweredOn); value != 1 {
		t.Fatalf("replay ended in different state: %v", value)
	}
}
