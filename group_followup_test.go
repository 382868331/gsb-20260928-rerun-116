package automation

import (
	"path/filepath"
	"testing"
	"time"
)

// confirmRule 构造一个两步动作组：先开空调（要求设备确认），确认后再开灯。
func confirmRule() Rule {
	return Rule{
		ID:        "ac-rule",
		Condition: DeviceProperty{DeviceID: "bedroom", Name: "occupied", Operator: OpEq, Value: 1},
		Steps: []Step{
			{ID: "ac-on", Action: &SetStateAction{DeviceID: "ac", Name: PoweredOn, Value: 1}, ConfirmWithin: 3 * time.Minute},
			{ID: "lamp-on", Action: &SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 1}},
		},
	}
}

func countApplied(core *Core, deviceID, name string) int {
	count := 0
	for _, record := range core.AppliedActions() {
		if record.Action.DeviceID == deviceID && record.Action.Name == name {
			count++
		}
	}
	return count
}

// 动作已下发、等待设备确认时保存并重新打开：等待状态与期限原样保留，
// 不另起一组，自动下发的值不得触发后续步骤。
func TestGroupRestartPreservesWaitingConfirm(t *testing.T) {
	now := time.Unix(9000, 0)
	store := FileStore{Path: filepath.Join(t.TempDir(), "state.json")}
	core, _ := NewCore(store, func() time.Time { return now })
	rule := confirmRule()
	_ = core.AddRule(rule)
	_, _ = core.Process((VirtualDevice{ID: "bedroom"}).Event("occupied", 1, now))
	run := groupRunFor(t, core, "ac-rule")
	if run.Steps[0].Status != StepWaitingConfirm {
		t.Fatalf("expected waiting_confirm, got %+v", run.Steps)
	}
	runID := run.RunID

	reopened, _ := NewCore(store, func() time.Time { return now })
	_ = reopened.AddRule(rule)
	_, _ = reopened.Tick(now.Add(time.Minute))
	run = groupRunFor(t, reopened, "ac-rule")
	if run.RunID != runID || run.Steps[0].Status != StepWaitingConfirm {
		t.Fatalf("restart must preserve the waiting run: %+v", run)
	}
	if _, ok := reopened.Snapshot().Value("lamp", PoweredOn); ok {
		t.Fatal("subsequent step ran without device confirmation")
	}

	// 再次重启：仍不得重复下发已有动作。
	reopened2, _ := NewCore(store, func() time.Time { return now })
	_ = reopened2.AddRule(rule)
	_, _ = reopened2.Tick(now.Add(2 * time.Minute))
	if count := countApplied(reopened2, "ac", PoweredOn); count != 1 {
		t.Fatalf("ac action must not be re-executed across restarts, got %d", count)
	}

	// 迟到确认（窗口内）：设备上报后整组才推进，且不再补发 ac 动作。
	_, _ = reopened2.Process((VirtualDevice{ID: "ac"}).Event(PoweredOn, 1, now.Add(2*time.Minute+30*time.Second)))
	run = groupRunFor(t, reopened2, "ac-rule")
	if !run.Done || run.Steps[0].Status != StepConfirmed || run.Steps[1].Status != StepConfirmed {
		t.Fatalf("late confirmation should complete the run: %+v", run)
	}
	if count := countApplied(reopened2, "ac", PoweredOn); count != 1 {
		t.Fatalf("late confirmation must not re-dispatch the ac action, got %d", count)
	}
}

// 停机跨越确认窗口后重开：步骤只能记为不确定；规则再次触发时
// 不得把自己写入的目标值当作真实效果而跳过确认执行后续步骤。
func TestGroupRestartPastDeadlineThenRetrigger(t *testing.T) {
	now := time.Unix(9600, 0)
	store := FileStore{Path: filepath.Join(t.TempDir(), "state.json")}
	core, _ := NewCore(store, func() time.Time { return now })
	rule := confirmRule()
	_ = core.AddRule(rule)
	_, _ = core.Process((VirtualDevice{ID: "bedroom"}).Event("occupied", 1, now))

	reopened, _ := NewCore(store, func() time.Time { return now })
	_ = reopened.AddRule(rule)
	_, _ = reopened.Tick(now.Add(10 * time.Minute)) // 恢复时确认窗口已过
	run := groupRunFor(t, reopened, "ac-rule")
	// 旧运行只能记为不确定并停止；条件仍成立时新运行重新等待设备回报，
	// 而不是把自己写入的 ac=1 当作已确认。
	uncertain := findGroupDecision(reopened.Decisions(), ReasonGroupCanceled)
	if uncertain == nil {
		t.Fatal("missed confirmation must be recorded as uncertain cancellation")
	}
	if run.Done || run.Steps[0].Status != StepWaitingConfirm {
		t.Fatalf("retriggered run must wait for device report, got %+v", run)
	}
	if _, ok := reopened.Snapshot().Value("lamp", PoweredOn); ok {
		t.Fatal("lamp must not run after uncertain confirmation")
	}
	if count := countApplied(reopened, "ac", PoweredOn); count != 1 {
		t.Fatalf("retrigger must not re-execute the ac action, got %d", count)
	}

	// 设备真正上报后新运行才推进到后续步骤。
	_, _ = reopened.Process((VirtualDevice{ID: "ac"}).Event(PoweredOn, 1, now.Add(12*time.Minute)))
	run = groupRunFor(t, reopened, "ac-rule")
	if !run.Done || run.Steps[1].Status != StepConfirmed {
		t.Fatalf("device report should complete the retriggered run: %+v", run)
	}
	if count := countApplied(reopened, "ac", PoweredOn); count != 1 {
		t.Fatalf("ac action executed %d times, want 1", count)
	}
}

// 条件短暂失效再触发：不得把自动化自己写入的值当作已满足而跳过确认。
func TestGroupRetriggerDoesNotTrustAutomationValue(t *testing.T) {
	now := time.Unix(9700, 0)
	core, _ := NewCore(nil, func() time.Time { return now })
	_ = core.AddRule(confirmRule())
	_, _ = core.Process((VirtualDevice{ID: "bedroom"}).Event("occupied", 1, now))
	_, _ = core.Process((VirtualDevice{ID: "bedroom"}).Event("occupied", 0, now.Add(time.Minute)))
	_, _ = core.Process((VirtualDevice{ID: "bedroom"}).Event("occupied", 1, now.Add(2*time.Minute)))

	run := groupRunFor(t, core, "ac-rule")
	if run.Done || run.Steps[0].Status != StepWaitingConfirm {
		t.Fatalf("retriggered run must wait for device confirmation: %+v", run)
	}
	if _, ok := core.Snapshot().Value("lamp", PoweredOn); ok {
		t.Fatal("automation-written value treated as confirmed effect")
	}
	if count := countApplied(core, "ac", PoweredOn); count != 1 {
		t.Fatalf("ac action executed %d times, want 1", count)
	}
	// 历史必须区分下发、确认与未知。
	if findGroupDecision(core.Decisions(), ReasonStepWaiting) == nil {
		t.Fatal("waiting-for-confirmation decision missing from history")
	}
	if findGroupDecision(core.Decisions(), ReasonGroupCanceled) == nil {
		t.Fatal("condition-false cancellation missing from history")
	}
}
