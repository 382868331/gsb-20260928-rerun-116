package automation

import (
	"testing"
	"time"
)

func floatPtr(v float64) *float64 { return &v }

func TestConflictingActionsArbitratedByPriority(t *testing.T) {
	now := time.Unix(1000, 0)
	core, err := NewCore(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	daylight := VirtualDevice{ID: "daylight", Types: []string{"light-sensor"}}
	rules := []Rule{
		{
			ID:        "night-low-priority",
			Priority:  1,
			Condition: DeviceProperty{DeviceID: "daylight", Name: "lux", Operator: OpLt, Value: 50},
			Action:    SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 1},
		},
		{
			ID:        "scene-off-high-priority",
			Priority:  10,
			Condition: DeviceProperty{DeviceID: "mode", Name: "away", Operator: OpEq, Value: 1},
			Action:    SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 0},
		},
	}
	for _, rule := range rules {
		if err := core.AddRule(rule); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := core.Process(daylight.Event("lux", 10, now)); err != nil {
		t.Fatal(err)
	}
	if value, _ := core.Snapshot().Value("lamp", PoweredOn); value != 1 {
		t.Fatalf("night rule should switch lamp on first: %v", value)
	}

	if _, err := core.Process((&VirtualDevice{ID: "mode"}).Event("away", 1, now.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	if value, _ := core.Snapshot().Value("lamp", PoweredOn); value != 0 {
		t.Fatalf("higher-priority away rule must win: %v", value)
	}

	// 同优先级时规则 ID 较小者稳定获胜：让两条规则在同一时刻竞争同一属性，
	// a-rule 即时置 1，b-rule 延迟置 0，裁决必须保留 a-rule 的动作。
	now2 := time.Unix(2000, 0)
	core2, _ := NewCore(nil, func() time.Time { return now2 })
	_ = core2.AddRule(Rule{ID: "a-rule", Condition: DeviceProperty{DeviceID: "s", Name: "x", Operator: OpEq, Value: 1}, Action: SetStateAction{DeviceID: "d", Name: PoweredOn, Value: 1, After: time.Minute}})
	_ = core2.AddRule(Rule{ID: "b-rule", Condition: DeviceProperty{DeviceID: "s", Name: "x", Operator: OpEq, Value: 1}, Action: SetStateAction{DeviceID: "d", Name: PoweredOn, Value: 0, After: time.Minute}})
	_, _ = core2.Process((&VirtualDevice{ID: "s"}).Event("x", 1, now2))
	pending := core2.PendingActions()
	if len(pending) != 1 || pending[0].RuleID != "a-rule" {
		t.Fatalf("same-priority tie should keep a-rule, got %+v", pending)
	}
	if got := findDecision(core2.Decisions(), ReasonSuperseded); got == nil {
		t.Fatal("expected superseded decision for b-rule")
	}
}

func TestManualOverrideTTLExpiresAndStaleActionIsCanceled(t *testing.T) {
	now := time.Unix(1100, 0)
	core, err := NewCore(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sensor := VirtualDevice{ID: "daylight", Types: []string{"light-sensor"}}
	rule := Rule{
		ID:        "night-light",
		Condition: DeviceProperty{DeviceID: "daylight", Name: "dark", Operator: OpEq, Value: 1},
		Duration:  10 * time.Minute,
		ManualTTL: 5 * time.Minute,
		Action:    SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 1, After: 2 * time.Minute},
	}
	if err := core.AddRule(rule); err != nil {
		t.Fatal(err)
	}

	// 夜间条件持续 10 分钟后触发，产生 2 分钟后开灯的延迟动作。
	_, _ = core.Process(sensor.Event("dark", 1, now))
	_, _ = core.Tick(now.Add(10 * time.Minute))
	if len(core.PendingActions()) != 1 {
		t.Fatal("expected delayed lamp-on action")
	}

	// 1 分钟后人工关灯：挂起动作被取消，规则进入 5 分钟人工覆盖。
	manualAt := now.Add(11 * time.Minute)
	if _, err := core.Process(ManualControl("lamp", PoweredOn, 0, manualAt)); err != nil {
		t.Fatal(err)
	}
	if got := findDecision(core.Decisions(), ReasonManualIntervention); got == nil {
		t.Fatal("manual cancellation missing from decision log")
	}
	if value, _ := core.Snapshot().Value("lamp", PoweredOn); value != 0 {
		t.Fatalf("lamp should stay manually off: %v", value)
	}

	// 覆盖期内即使推进时间也不会自动开灯。
	_, _ = core.Tick(manualAt.Add(4 * time.Minute))
	if value, _ := core.Snapshot().Value("lamp", PoweredOn); value != 0 {
		t.Fatalf("automation fired inside manual TTL: %v", value)
	}

	// TTL 到期后需要重新看到条件并再次持续 10 分钟才触发；
	// 手动操作结束后不会直接执行早已失效的旧动作。
	expireAt := manualAt.Add(5 * time.Minute)
	_, _ = core.Tick(expireAt)
	_, _ = core.Tick(expireAt.Add(10 * time.Minute))
	if len(core.PendingActions()) != 1 {
		t.Fatalf("expected a freshly scheduled action after TTL expiry, got %d", len(core.PendingActions()))
	}
	_, _ = core.Tick(expireAt.Add(12 * time.Minute))
	if value, _ := core.Snapshot().Value("lamp", PoweredOn); value != 1 {
		t.Fatalf("lamp should turn on after fresh duration: %v", value)
	}
}

func TestDelayedActionCanceledWhenConditionFalseBeforeDeadline(t *testing.T) {
	now := time.Unix(1200, 0)
	core, err := NewCore(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sensor := VirtualDevice{ID: "daylight"}
	_ = core.AddRule(Rule{
		ID:        "night-light",
		Condition: DeviceProperty{DeviceID: "daylight", Name: "dark", Operator: OpEq, Value: 1},
		Action:    SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 1, After: 5 * time.Minute},
	})
	_, _ = core.Process(sensor.Event("dark", 1, now))
	if len(core.PendingActions()) != 1 {
		t.Fatal("expected scheduled action")
	}
	// 延迟窗口内夜间条件消失：动作到点时必须取消。
	_, _ = core.Process(sensor.Event("dark", 0, now.Add(time.Minute)))
	_, _ = core.Tick(now.Add(6 * time.Minute))
	if _, ok := core.Snapshot().Value("lamp", PoweredOn); ok {
		t.Fatal("stale action executed after condition ended")
	}
	if got := findDecision(core.Decisions(), ReasonConditionFalse); got == nil {
		t.Fatal("expected condition_false cancellation decision")
	}
}

func TestReplayReproducesDecisions(t *testing.T) {
	now := time.Unix(1300, 0)
	core, err := NewCore(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sensor := VirtualDevice{ID: "daylight"}
	rule := Rule{
		ID:        "night-light",
		Condition: DeviceProperty{DeviceID: "daylight", Name: "dark", Operator: OpEq, Value: 1},
		Duration:  time.Minute,
		Action:    SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 1, After: 5 * time.Minute},
	}
	if err := core.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	_, _ = core.Process(sensor.Event("dark", 1, now))
	_, _ = core.Tick(now.Add(time.Minute))
	_, _ = core.Process(ManualControl("lamp", PoweredOn, 0, now.Add(2*time.Minute)))

	history := core.History()
	cutoff := now.Add(2 * time.Minute)
	replayed, decisions, err := Replay(nil, []Rule{rule}, history, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := replayed.Snapshot().Value("lamp", PoweredOn); value != 0 {
		t.Fatalf("replay ended in different state: %v", value)
	}
	if findDecision(decisions, ReasonConditionHeld) == nil {
		t.Fatal("replay missing executed decision")
	}
	last := decisions[len(decisions)-1]
	if last.Reason != ReasonManualIntervention {
		t.Fatalf("replay decision log mismatch: %+v", last)
	}
}

func findDecision(decisions []Decision, reason Reason) *Decision {
	for i := range decisions {
		if decisions[i].Reason == reason {
			return &decisions[i]
		}
	}
	return nil
}
