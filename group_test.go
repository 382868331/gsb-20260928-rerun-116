package automation

import (
	"path/filepath"
	"testing"
	"time"
)

func groupByID(t *testing.T, core *Core, id string) GroupRun {
	t.Helper()
	for _, group := range core.Groups() {
		if group.ID == id {
			return group
		}
	}
	t.Fatalf("group %s not found in %+v", id, core.Groups())
	return GroupRun{}
}

func secureCoolRule() Rule {
	return Rule{
		ID:        "secure-cool",
		Priority:  5,
		Condition: DeviceProperty{DeviceID: "mode", Name: "away", Operator: OpEq, Value: 1},
		Duration:  time.Minute,
		Steps: []Step{
			{Action: SetStateAction{DeviceID: "window", Name: ContactOpen, Value: 0, After: time.Minute}, ConfirmTimeout: 3 * time.Minute},
			{Action: SetStateAction{DeviceID: "ac", Name: PoweredOn, Value: 1, After: 2 * time.Minute}},
		},
	}
}

// 动作组按序执行：第一步经设备回执确认后才下发第二步；
// 每一步区分 目标已下发 / 已确认 / 不确定，整组不会因触发就直接记成完成。
func TestGroupStepsConfirmSequentially(t *testing.T) {
	now := time.Unix(10000, 0)
	core, err := NewCore(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	mode := VirtualDevice{ID: "mode"}
	window := VirtualDevice{ID: "window", Types: []string{"window"}}
	mustAddRule(t, core, secureCoolRule())

	mustProcess(t, core, mode.Event("away", 1, now))
	mustTick(t, core, now.Add(time.Minute))
	groups := core.Groups()
	if len(groups) != 1 {
		t.Fatalf("expected one group run, got %+v", groups)
	}
	run := groups[0]
	if run.Steps[0].State != StepPending || run.Steps[1].State != StepPending {
		t.Fatalf("steps should be preparing, got %+v", run.Steps)
	}

	// 第一步到点：目标已下发，等待窗户回执；第二步不得提前下发。
	mustTick(t, core, now.Add(2*time.Minute))
	run = groupByID(t, core, run.ID)
	if run.Steps[0].State != StepIssued {
		t.Fatalf("step 1 should be issued awaiting confirmation, got %s", run.Steps[0].State)
	}
	if run.Steps[1].State != StepPending || run.Steps[1].ActionID != "" {
		t.Fatalf("step 2 must not start before step 1 confirms: %+v", run.Steps[1])
	}
	if _, ok := core.Snapshot().Value("ac", PoweredOn); ok {
		t.Fatal("ac switched on before window close was confirmed")
	}

	// 窗户回执确认关闭：第一步确认，第二步才排定。
	mustProcess(t, core, window.Contact(false, now.Add(3*time.Minute)))
	run = groupByID(t, core, run.ID)
	if run.Steps[0].State != StepConfirmed {
		t.Fatalf("step 1 should be confirmed by device report, got %s", run.Steps[0].State)
	}
	mustTick(t, core, now.Add(5*time.Minute))
	run = groupByID(t, core, run.ID)
	if run.Steps[1].State != StepConfirmed {
		t.Fatalf("step 2 should complete without confirm requirement, got %s", run.Steps[1].State)
	}
	if value, _ := core.Snapshot().Value("ac", PoweredOn); value != 1 {
		t.Fatal("ac should turn on after window confirmed closed")
	}
	confirmed := decisionsWithReason(core.Decisions(), ReasonStepConfirmed)
	if len(confirmed) != 2 || confirmed[0].GroupID != run.ID {
		t.Fatalf("expected per-step confirmation decisions traceable to the group: %+v", confirmed)
	}
}

// 准备阶段被人工接管：已确认的第一步保留，后续步骤停止并标注接管层；
// 撤销接管后按当前观察重新判断——已满足的步骤跳过，只执行仍需要的步骤，
// 已取消的旧步骤不补发。
func TestGroupTakeoverDuringPreparationAndReobserve(t *testing.T) {
	now := time.Unix(11000, 0)
	core, err := NewCore(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	mode := VirtualDevice{ID: "mode"}
	window := VirtualDevice{ID: "window"}
	mustAddRule(t, core, secureCoolRule())

	mustProcess(t, core, mode.Event("away", 1, now))
	mustTick(t, core, now.Add(time.Minute))
	first := core.Groups()[0]
	mustTick(t, core, now.Add(2*time.Minute))                           // 第一步下发
	mustProcess(t, core, window.Contact(false, now.Add(3*time.Minute))) // 第一步确认
	first = groupByID(t, core, first.ID)
	if first.Steps[0].State != StepConfirmed || first.Steps[1].State != StepPending {
		t.Fatalf("expected partial completion (step1 confirmed, step2 pending): %+v", first.Steps)
	}

	// 第二步准备期间人工接管 ac：后续动作停止。
	if err := core.Takeover(Override{ID: "guest-ac", Scope: OverrideScope{DeviceID: "ac"}}, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	first = groupByID(t, core, first.ID)
	if first.Steps[0].State != StepConfirmed || first.Steps[1].State != StepCanceled {
		t.Fatalf("takeover must stop remaining steps only: %+v", first.Steps)
	}
	if first.Steps[1].OverrideID != "guest-ac" {
		t.Fatalf("canceled step must record the override layer: %+v", first.Steps[1])
	}
	mustTick(t, core, now.Add(6*time.Minute))
	if _, ok := core.Snapshot().Value("ac", PoweredOn); ok {
		t.Fatal("canceled step executed during takeover")
	}

	// 撤销接管：规则重新观察条件并再次触发；新的一次组运行里
	// 窗户已关闭（当前观察）→ 第一步跳过，只执行仍需要的第二步；
	// 旧运行里被取消的步骤不补发。
	if err := core.RevokeOverride("guest-ac", now.Add(7*time.Minute)); err != nil {
		t.Fatal(err)
	}
	mustTick(t, core, now.Add(8*time.Minute))
	mustTick(t, core, now.Add(8*time.Minute+time.Second))
	groups := core.Groups()
	if len(groups) != 2 {
		t.Fatalf("expected a fresh group run after revoke, got %+v", groups)
	}
	second := groups[1]
	if second.Steps[0].State != StepSkipped {
		t.Fatalf("already-closed window step should be skipped by observation, got %s", second.Steps[0].State)
	}
	mustTick(t, core, now.Add(10*time.Minute+time.Second))
	second = groupByID(t, core, second.ID)
	if second.Steps[1].State != StepConfirmed {
		t.Fatalf("only the still-needed step should execute, got %+v", second.Steps)
	}
	if value, _ := core.Snapshot().Value("ac", PoweredOn); value != 1 {
		t.Fatal("ac should turn on from the fresh group run")
	}
	first = groupByID(t, core, first.ID)
	if first.Steps[1].State != StepCanceled {
		t.Fatalf("stale canceled step must not be reissued: %+v", first.Steps[1])
	}
	if got := len(decisionsWithReason(core.Decisions(), ReasonAlreadySatisfied)); got != 1 {
		t.Fatalf("expected one already_satisfied decision, got %d", got)
	}
}

// 确认期限内设备离线且未回执：步骤记为不确定，后续步骤中止。
func TestGroupStepUncertainWhenUnconfirmed(t *testing.T) {
	now := time.Unix(12000, 0)
	core, err := NewCore(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	mode := VirtualDevice{ID: "mode"}
	window := VirtualDevice{ID: "window"}
	mustAddRule(t, core, secureCoolRule())

	mustProcess(t, core, mode.Event("away", 1, now))
	mustTick(t, core, now.Add(time.Minute))
	run := core.Groups()[0]
	mustTick(t, core, now.Add(2*time.Minute)) // 第一步下发，确认期限 t+5m
	mustProcess(t, core, window.Availability(false, now.Add(3*time.Minute)))
	mustTick(t, core, now.Add(6*time.Minute))
	run = groupByID(t, core, run.ID)
	if run.Steps[0].State != StepUncertain {
		t.Fatalf("unconfirmed step must be uncertain, got %s", run.Steps[0].State)
	}
	if run.Steps[1].State != StepCanceled {
		t.Fatalf("later steps must halt when prerequisite is uncertain: %+v", run.Steps[1])
	}
	if _, ok := core.Snapshot().Value("ac", PoweredOn); ok {
		t.Fatal("ac switched on although window close was never confirmed")
	}
	uncertain := decisionsWithReason(core.Decisions(), ReasonDeviceOffline)
	if len(uncertain) == 0 || uncertain[0].Outcome != OutcomeUncertain {
		t.Fatalf("expected uncertain outcome decision: %+v", core.Decisions())
	}
}

// 两组共享设备：目标相同则等待并注明依据；独立设备的组继续推进。
func TestGroupsSharingDeviceWaitAndExplain(t *testing.T) {
	now := time.Unix(13000, 0)
	core, err := NewCore(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	mode := VirtualDevice{ID: "mode"}
	vent := VirtualDevice{ID: "vent"}
	mustAddRule(t, core, Rule{
		ID:        "group-a",
		Priority:  5,
		Condition: DeviceProperty{DeviceID: "mode", Name: "away", Operator: OpEq, Value: 1},
		Steps: []Step{
			{Action: SetStateAction{DeviceID: "vent", Name: PoweredOn, Value: 1}, ConfirmTimeout: 10 * time.Minute},
		},
	})
	mustAddRule(t, core, Rule{
		ID:        "group-b",
		Priority:  5,
		Condition: DeviceProperty{DeviceID: "mode", Name: "guest", Operator: OpEq, Value: 1},
		Steps: []Step{
			{Action: SetStateAction{DeviceID: "vent", Name: PoweredOn, Value: 1}, ConfirmTimeout: 10 * time.Minute},
			{Action: SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 1}},
		},
	})

	mustProcess(t, core, mode.Event("away", 1, now))
	mustProcess(t, core, mode.Event("guest", 1, now.Add(time.Second)))
	groups := core.Groups()
	if len(groups) != 2 {
		t.Fatalf("expected two group runs, got %+v", groups)
	}
	var waiting *Decision
	for i, decision := range core.Decisions() {
		if decision.Reason == ReasonWaitSharedDevice {
			waiting = &core.Decisions()[i]
		}
	}
	if waiting == nil || waiting.Detail == "" {
		t.Fatalf("shared-device wait must be explained: %+v", core.Decisions())
	}
	// 独立设备步骤不受共享等待影响：group-b 的第一步等待 vent，
	// 但等待只发生在共享属性上；vent 确认后等待的步骤继续推进。
	mustProcess(t, core, vent.Event(PoweredOn, 1, now.Add(2*time.Second)))
	second := groupByID(t, core, groups[1].ID)
	if second.Steps[0].State != StepSkipped {
		t.Fatalf("waiting step should resolve by observation after shared device confirmed: %+v", second.Steps[0])
	}
	if second.Steps[1].State != StepConfirmed {
		t.Fatalf("independent device step should proceed: %+v", second.Steps[1])
	}
	if value, _ := core.Snapshot().Value("lamp", PoweredOn); value != 1 {
		t.Fatal("independent device action did not proceed")
	}
}

// 重启后继续核对：组运行状态持久化，重启前已下发的步骤
// 在重启后仍能由设备回执确认并推进后续步骤。
func TestGroupRestartResumesVerification(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	now := time.Unix(14000, 0)
	store := FileStore{Path: statePath}
	core, err := NewCore(store, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	mode := VirtualDevice{ID: "mode"}
	window := VirtualDevice{ID: "window"}
	mustAddRule(t, core, secureCoolRule())

	mustProcess(t, core, mode.Event("away", 1, now))
	mustTick(t, core, now.Add(time.Minute))
	run := core.Groups()[0]
	mustTick(t, core, now.Add(2*time.Minute)) // 第一步下发，等待回执

	// 重启：同一状态文件恢复，第一步仍在等待确认。
	restarted, err := NewCore(store, func() time.Time { return now.Add(3 * time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	mustAddRule(t, restarted, secureCoolRule())
	run = groupByID(t, restarted, run.ID)
	if run.Steps[0].State != StepIssued {
		t.Fatalf("issued step must survive restart, got %s", run.Steps[0].State)
	}

	// 重启后设备回执到达：第一步确认，第二步继续推进。
	mustProcess(t, restarted, window.Contact(false, now.Add(4*time.Minute)))
	mustTick(t, restarted, now.Add(6*time.Minute))
	run = groupByID(t, restarted, run.ID)
	if run.Steps[0].State != StepConfirmed || run.Steps[1].State != StepConfirmed {
		t.Fatalf("group should complete after restart: %+v", run.Steps)
	}
	if value, _ := restarted.Snapshot().Value("ac", PoweredOn); value != 1 {
		t.Fatal("ac should turn on after post-restart confirmation")
	}
}
