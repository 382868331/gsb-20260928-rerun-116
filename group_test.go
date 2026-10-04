package automation

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func groupRule() Rule {
	return Rule{
		ID:       "bedtime-ac",
		Priority: 5,
		Condition: All{Conditions: []Condition{
			DeviceProperty{DeviceID: "thermostat", Name: TemperatureC, Operator: OpGt, Value: 26},
			DeviceProperty{DeviceID: "bedroom", Name: "occupied", Operator: OpEq, Value: 1},
		}},
		Duration: 2 * time.Minute,
		Steps: []Step{
			{ID: "confirm-window", Verify: &StepVerify{
				DeviceID: "window", Name: ContactOpen, Operator: OpEq, Value: 0,
				Within: 4 * time.Minute,
			}},
			{ID: "ac-on", Action: &SetStateAction{
				DeviceID: "ac", Name: PoweredOn, Value: 1, After: time.Minute,
			}, ConfirmWithin: 3 * time.Minute},
		},
	}
}

func groupRunFor(t *testing.T, core *Core, ruleID string) GroupRun {
	t.Helper()
	for _, run := range core.GroupRuns() {
		if run.RuleID == ruleID {
			return run
		}
	}
	t.Fatalf("no group run for %s", ruleID)
	return GroupRun{}
}

func findGroupDecision(decisions []Decision, reason Reason) *Decision {
	for i := range decisions {
		if decisions[i].Reason == reason {
			return &decisions[i]
		}
	}
	return nil
}

// 准备阶段（等待开窗确认/动作未落地）被人工接管时，后续步骤立即停止。
func TestGroupTakeoverDuringPreparationStopsRemainingSteps(t *testing.T) {
	now := time.Unix(3000, 0)
	core, _ := NewCore(nil, func() time.Time { return now })
	if err := core.AddRule(groupRule()); err != nil {
		t.Fatal(err)
	}
	thermostat := VirtualDevice{ID: "thermostat"}
	bedroom := VirtualDevice{ID: "bedroom", Room: "bedroom"}
	window := VirtualDevice{ID: "window", Room: "bedroom"}

	_, _ = core.Process(thermostat.Temperature(28, now))
	_, _ = core.Process(bedroom.Event("occupied", 1, now))
	_, _ = core.Process(window.Contact(true, now)) // 窗未关：组进入准备阶段
	_, _ = core.Tick(now.Add(2 * time.Minute))     // 条件持续满足，组启动，等待窗关确认

	run := groupRunFor(t, core, "bedtime-ac")
	if run.Steps[0].Status != StepWaitingConfirm {
		t.Fatalf("step should be waiting for window confirmation: %+v", run.Steps)
	}

	// 准备阶段人工接管卧室：等待中的确认与后续动作全部停止。
	if err := core.Takeover(Override{ID: "guest", Scope: OverrideScope{Room: "bedroom"}}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	run = groupRunFor(t, core, "bedtime-ac")
	if !run.Done || run.Steps[0].Status != StepCanceled || run.Steps[1].Status != StepCanceled {
		t.Fatalf("takeover should cancel remaining steps: %+v", run.Steps)
	}
	canceled := findGroupDecision(core.Decisions(), ReasonGroupCanceled)
	if canceled == nil || canceled.OverrideID != "guest" {
		t.Fatalf("group cancellation must name the override layer: %+v", canceled)
	}

	// 接管期内关窗也不得推进任何步骤。
	_, _ = core.Process(window.Contact(false, now.Add(4*time.Minute)))
	_, _ = core.Tick(now.Add(10 * time.Minute))
	if _, ok := core.Snapshot().Value("ac", PoweredOn); ok {
		t.Fatal("ac must stay untouched while overridden")
	}
}

// 部分完成后取消；撤销接管后由当前观察重新判断，已满足步骤跳过、
// 失去时效的旧动作不补发。
func TestGroupRevokeReevaluatesFromCurrentObservation(t *testing.T) {
	now := time.Unix(4000, 0)
	core, _ := NewCore(nil, func() time.Time { return now })
	if err := core.AddRule(groupRule()); err != nil {
		t.Fatal(err)
	}
	thermostat := VirtualDevice{ID: "thermostat"}
	bedroom := VirtualDevice{ID: "bedroom", Room: "bedroom"}
	window := VirtualDevice{ID: "window", Room: "bedroom"}
	ac := VirtualDevice{ID: "ac", Room: "bedroom"}

	_, _ = core.Process(thermostat.Temperature(28, now))
	_, _ = core.Process(bedroom.Event("occupied", 1, now))
	_, _ = core.Process(window.Contact(false, now)) // 窗已关
	_, _ = core.Tick(now.Add(2 * time.Minute))      // 组启动：步骤1确认，步骤2排定 t+3m

	run := groupRunFor(t, core, "bedtime-ac")
	if run.Steps[0].Status != StepConfirmed {
		t.Fatalf("window step should be confirmed: %+v", run.Steps)
	}
	pending := core.PendingActions()
	if len(pending) != 1 {
		t.Fatalf("expected pending ac action, got %+v", pending)
	}
	staleActionID := pending[0].ID

	// 步骤1已完成、步骤2未落地时人工接管：部分完成的组被取消。
	if err := core.Takeover(Override{ID: "guest", Scope: OverrideScope{DeviceID: "ac"}}, now.Add(2*time.Minute+30*time.Second)); err != nil {
		t.Fatal(err)
	}
	run = groupRunFor(t, core, "bedtime-ac")
	if !run.Done || run.Steps[0].Status != StepConfirmed || run.Steps[1].Status != StepCanceled {
		t.Fatalf("partially completed group should cancel remaining steps: %+v", run.Steps)
	}

	// 撤销接管：规则重新观察条件，再次持续 2 分钟后开启新运行。
	if err := core.RevokeOverride("guest", now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, _ = core.Tick(now.Add(5 * time.Minute))
	run = groupRunFor(t, core, "bedtime-ac")
	if run.RunID == "" || run.Steps[0].Status != StepConfirmed {
		t.Fatalf("new run should re-confirm window from current observation: %+v", run.Steps)
	}
	// 旧动作不得补发：新运行排定的是全新动作 ID。
	for _, action := range core.PendingActions() {
		if action.ID == staleActionID {
			t.Fatalf("stale action %s must not be reissued", staleActionID)
		}
	}
	for _, record := range core.AppliedActions() {
		if record.Action.ID == staleActionID {
			t.Fatalf("stale action %s must not execute", staleActionID)
		}
	}

	// 新运行的动作到点落地，并由设备上报确认后整组才记为完成。
	_, _ = core.Tick(now.Add(6 * time.Minute))
	run = groupRunFor(t, core, "bedtime-ac")
	if run.Done {
		t.Fatal("group must not complete before device confirms the effect")
	}
	_, _ = core.Process(ac.Event(PoweredOn, 1, now.Add(6*time.Minute+30*time.Second)))
	run = groupRunFor(t, core, "bedtime-ac")
	if !run.Done || run.Steps[1].Status != StepConfirmed {
		t.Fatalf("group should complete only after device confirmation: %+v", run.Steps)
	}
	if findGroupDecision(core.Decisions(), ReasonGroupCompleted) == nil {
		t.Fatal("missing group_completed decision")
	}
}

// 重启后继续核对：确认等待状态持久化，恢复后由设备上报完成确认。
func TestGroupRestartContinuesVerification(t *testing.T) {
	now := time.Unix(5000, 0)
	store := FileStore{Path: filepath.Join(t.TempDir(), "state.json")}
	core, _ := NewCore(store, func() time.Time { return now })
	rule := groupRule()
	if err := core.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	_, _ = core.Process((VirtualDevice{ID: "thermostat"}).Temperature(28, now))
	_, _ = core.Process((VirtualDevice{ID: "bedroom"}).Event("occupied", 1, now))
	_, _ = core.Process((VirtualDevice{ID: "window"}).Contact(false, now))
	_, _ = core.Tick(now.Add(2 * time.Minute)) // 组启动，排定 ac 动作
	_, _ = core.Tick(now.Add(3 * time.Minute)) // ac 动作落地，进入设备确认等待

	run := groupRunFor(t, core, "bedtime-ac")
	if run.Steps[1].Status != StepWaitingConfirm {
		t.Fatalf("step should wait for device confirmation: %+v", run.Steps)
	}

	// 模拟进程重启：从同一状态文件恢复，继续核对未完成的确认。
	restarted, err := NewCore(store, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	run = groupRunFor(t, restarted, "bedtime-ac")
	if run.Steps[1].Status != StepWaitingConfirm {
		t.Fatalf("restart must preserve confirmation wait: %+v", run.Steps)
	}
	_, _ = restarted.Process((VirtualDevice{ID: "ac"}).Event(PoweredOn, 1, now.Add(4*time.Minute)))
	run = groupRunFor(t, restarted, "bedtime-ac")
	if !run.Done || run.Steps[1].Status != StepConfirmed {
		t.Fatalf("restart should complete verification: %+v", run.Steps)
	}
}

// 停机跨越确认窗口：恢复后步骤记为不确定而不是当作完成。
func TestGroupConfirmDeadlineMissedDuringDowntime(t *testing.T) {
	now := time.Unix(6000, 0)
	store := FileStore{Path: filepath.Join(t.TempDir(), "state.json")}
	core, _ := NewCore(store, func() time.Time { return now })
	rule := groupRule()
	_ = core.AddRule(rule)
	_, _ = core.Process((VirtualDevice{ID: "thermostat"}).Temperature(28, now))
	_, _ = core.Process((VirtualDevice{ID: "bedroom"}).Event("occupied", 1, now))
	_, _ = core.Process((VirtualDevice{ID: "window"}).Contact(false, now))
	_, _ = core.Tick(now.Add(2 * time.Minute))
	_, _ = core.Tick(now.Add(3 * time.Minute)) // ac 落地，确认窗口 3 分钟

	restarted, _ := NewCore(store, func() time.Time { return now })
	_ = restarted.AddRule(rule)
	_, _ = restarted.Tick(now.Add(10 * time.Minute)) // 恢复时窗口已过
	run := groupRunFor(t, restarted, "bedtime-ac")
	if !run.Done || run.Steps[1].Status != StepUncertain {
		t.Fatalf("missed confirmation must be uncertain, not complete: %+v", run.Steps)
	}
	uncertain := findGroupDecision(restarted.Decisions(), ReasonGroupCanceled)
	if uncertain == nil || !strings.Contains(uncertain.Detail, "unconfirmed") {
		t.Fatalf("expected uncertainty detail, got %+v", uncertain)
	}
}

// 多组共享设备时冲突裁决说明依据；独立设备的组照常推进。
func TestGroupSharedDeviceConflictExplained(t *testing.T) {
	now := time.Unix(7000, 0)
	core, _ := NewCore(nil, func() time.Time { return now })
	acRule := groupRule()
	acRule.Steps = []Step{{ID: "ac-on", Action: &SetStateAction{
		DeviceID: "ac", Name: PoweredOn, Value: 1, After: time.Minute,
	}}}
	ecoRule := Rule{
		ID:        "eco-off",
		Priority:  9,
		Condition: DeviceProperty{DeviceID: "mode", Name: "eco", Operator: OpEq, Value: 1},
		Steps: []Step{{ID: "ac-off", Action: &SetStateAction{
			DeviceID: "ac", Name: PoweredOn, Value: 0, After: time.Minute,
		}}},
	}
	indicatorRule := Rule{
		ID:        "indicator-on",
		Condition: DeviceProperty{DeviceID: "mode", Name: "eco", Operator: OpEq, Value: 1},
		Steps: []Step{{ID: "lamp", Action: &SetStateAction{
			DeviceID: "indicator", Name: PoweredOn, Value: 1,
		}}},
	}
	for _, rule := range []Rule{acRule, ecoRule, indicatorRule} {
		if err := core.AddRule(rule); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = core.Process((VirtualDevice{ID: "thermostat"}).Temperature(28, now))
	_, _ = core.Process((VirtualDevice{ID: "bedroom"}).Event("occupied", 1, now))
	_, _ = core.Process((VirtualDevice{ID: "window"}).Contact(false, now))
	_, _ = core.Tick(now.Add(2 * time.Minute)) // bedtime-ac 排定 ac=1
	_, _ = core.Process((VirtualDevice{ID: "mode"}).Event("eco", 1, now.Add(2*time.Minute)))

	// 共享 ac：eco-off 优先级更高，bedtime-ac 的动作被取代且说明依据。
	superseded := findGroupDecision(core.Decisions(), ReasonSuperseded)
	if superseded == nil || !strings.Contains(superseded.Detail, "eco-off") {
		t.Fatalf("conflict decision must explain the winner: %+v", superseded)
	}
	run := groupRunFor(t, core, "bedtime-ac")
	if !run.Done || run.Steps[0].Status != StepCanceled {
		t.Fatalf("losing group should stop: %+v", run.Steps)
	}
	// 独立设备的组不受影响，照常落地。
	if value, _ := core.Snapshot().Value("indicator", PoweredOn); value != 1 {
		t.Fatalf("independent device group should proceed: %v", value)
	}
}

// 离线设备：确认不可见，窗口过后步骤记为不确定，后续动作不下发。
func TestGroupOfflineDeviceUncertain(t *testing.T) {
	now := time.Unix(8000, 0)
	core, _ := NewCore(nil, func() time.Time { return now })
	if err := core.AddRule(groupRule()); err != nil {
		t.Fatal(err)
	}
	window := VirtualDevice{ID: "window"}
	_, _ = core.Process((VirtualDevice{ID: "thermostat"}).Temperature(28, now))
	_, _ = core.Process((VirtualDevice{ID: "bedroom"}).Event("occupied", 1, now))
	_, _ = core.Process(window.Contact(false, now))
	_, _ = core.Process(window.Availability(false, now.Add(time.Minute))) // 窗传感器离线
	_, _ = core.Tick(now.Add(2 * time.Minute))                            // 组启动，等待确认
	_, _ = core.Tick(now.Add(7 * time.Minute))                            // 确认窗口（4m）已过

	run := groupRunFor(t, core, "bedtime-ac")
	if !run.Done || run.Steps[0].Status != StepUncertain || run.Steps[1].Status != StepCanceled {
		t.Fatalf("offline device should make step uncertain: %+v", run.Steps)
	}
	if _, ok := core.Snapshot().Value("ac", PoweredOn); ok {
		t.Fatal("ac action must not be issued after uncertain verification")
	}
}
