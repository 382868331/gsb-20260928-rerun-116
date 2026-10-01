package automation

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func lampRule() Rule {
	return Rule{
		ID:        "night-light",
		Condition: DeviceProperty{DeviceID: "daylight", Name: "dark", Operator: OpEq, Value: 1},
		Duration:  time.Minute,
		Action:    SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 1},
	}
}

func TestLayeredTakeoversRestoreIndependently(t *testing.T) {
	now := time.Unix(3000, 0)
	core, err := NewCore(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := core.AddRule(lampRule()); err != nil {
		t.Fatal(err)
	}
	lamp := VirtualDevice{ID: "lamp", Room: "living"}
	sensor := VirtualDevice{ID: "daylight"}
	_, _ = core.Process(lamp.Event(PoweredOn, 0, now)) // 注册房间信息
	_, _ = core.Process(sensor.Event("dark", 1, now))

	// 两层接管叠加：整个客厅 10 分钟 + 灯的临时例外 30 分钟。
	roomID, err := core.BeginTakeover(Takeover{Room: "living", TTL: 10 * time.Minute}, now)
	if err != nil {
		t.Fatal(err)
	}
	deviceID, err := core.BeginTakeover(Takeover{DeviceID: "lamp", Name: PoweredOn, TTL: 30 * time.Minute}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(core.ActiveTakeovers()) != 2 {
		t.Fatal("expected two stacked takeover layers")
	}

	// 房间层到期后，设备例外仍然生效：规则不能触发。
	if _, err := core.Tick(now.Add(15 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if value, _ := core.Snapshot().Value("lamp", PoweredOn); value != 0 {
		t.Fatalf("device exception should still block after room takeover expired: %v", value)
	}
	if len(core.ActiveTakeovers()) != 1 || core.ActiveTakeovers()[0].ID != deviceID {
		t.Fatalf("only device exception should remain: %+v", core.ActiveTakeovers())
	}

	// 撤销设备例外后，规则从当前时刻重新观察条件，持续满足后才触发。
	if err := core.ReleaseTakeover(deviceID, now.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Tick(now.Add(20*time.Minute + 30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if value, _ := core.Snapshot().Value("lamp", PoweredOn); value != 0 {
		t.Fatal("rule must re-observe condition after release, not fire immediately")
	}
	if _, err := core.Tick(now.Add(21 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if value, _ := core.Snapshot().Value("lamp", PoweredOn); value != 1 {
		t.Fatalf("rule should fire one minute after release: %v", value)
	}

	// 决策日志能解释每一层的建立与结束。
	var begin, expired, released int
	for _, decision := range core.Decisions() {
		switch {
		case decision.Outcome == OutcomeTakeoverBegin:
			begin++
			if decision.TakeoverID != roomID && decision.TakeoverID != deviceID {
				t.Fatalf("unexpected takeover id in log: %+v", decision)
			}
		case decision.Outcome == OutcomeTakeoverEnd && decision.Reason == ReasonTakeoverExpired:
			expired++
		case decision.Outcome == OutcomeTakeoverEnd && decision.Reason == ReasonTakeoverReleased:
			released++
		}
	}
	if begin != 2 || expired != 1 || released != 1 {
		t.Fatalf("takeover audit trail incomplete: begin=%d expired=%d released=%d", begin, expired, released)
	}
}

func TestRestartAcrossTakeoverExpiryDoesNotReplayStaleActions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "var", "automation.json")
	now := time.Unix(4000, 0)
	core, err := NewCore(FileStore{Path: path}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	rule := lampRule()
	rule.Action.After = 2 * time.Minute
	if err := core.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	sensor := VirtualDevice{ID: "daylight"}
	_, _ = core.Process(sensor.Event("dark", 1, now))
	_, _ = core.Tick(now.Add(time.Minute)) // 触发，动作应于 now+3m 落地
	if len(core.PendingActions()) != 1 {
		t.Fatal("expected pending action")
	}
	// 一层与 lamp 无关的房间接管，TTL 将在停机期间到期。
	if _, err := core.BeginTakeover(Takeover{Room: "bedroom", TTL: 5 * time.Minute}, now.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	// 条件在停机前消失：动作到点时必须取消而不是补发。
	_, _ = core.Process(sensor.Event("dark", 0, now.Add(2*time.Minute)))

	// 进程停止，直到 now+30m 才重启：接管（now+6.5m 到期）与动作（now+3m 到期）
	// 都跨越了停机期间。
	restarted, err := NewCore(FileStore{Path: path}, func() time.Time { return now.Add(30 * time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Tick(now.Add(30 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, ok := restarted.Snapshot().Value("lamp", PoweredOn); ok {
		t.Fatal("stale action must not be replayed after downtime")
	}
	if len(restarted.ActiveTakeovers()) != 0 {
		t.Fatal("takeover should have expired during downtime")
	}
	var staleCancel, takeoverExpired *Decision
	for i, decision := range restarted.Decisions() {
		if decision.Outcome == OutcomeCanceled && decision.Reason == ReasonConditionFalse && decision.DueAt != nil {
			staleCancel = &restarted.Decisions()[i]
		}
		if decision.Outcome == OutcomeTakeoverEnd && decision.Reason == ReasonTakeoverExpired {
			takeoverExpired = &restarted.Decisions()[i]
		}
	}
	if staleCancel == nil {
		t.Fatal("expected canceled decision with DueAt marking the missed deadline")
	}
	if !staleCancel.DueAt.Equal(now.Add(3 * time.Minute)) {
		t.Fatalf("DueAt should record when the action was due: %v", staleCancel.DueAt)
	}
	if takeoverExpired == nil {
		t.Fatal("expected takeover expiry decision after restart")
	}
}

func TestOfflineDeviceActionNotExecuted(t *testing.T) {
	now := time.Unix(5000, 0)
	core, err := NewCore(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	rule := lampRule()
	rule.Action.After = 2 * time.Minute
	if err := core.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	sensor := VirtualDevice{ID: "daylight"}
	lamp := VirtualDevice{ID: "lamp"}
	_, _ = core.Process(sensor.Event("dark", 1, now))
	_, _ = core.Tick(now.Add(time.Minute)) // 排定 now+3m 开灯
	_, _ = core.Process(lamp.Status(false, now.Add(2*time.Minute)))
	_, _ = core.Tick(now.Add(5 * time.Minute))
	if _, ok := core.Snapshot().Value("lamp", PoweredOn); ok {
		t.Fatal("action executed on offline device")
	}
	if got := findDecision(core.Decisions(), ReasonDeviceOffline); got == nil {
		t.Fatal("expected device_offline decision explaining the skipped action")
	}
}

func TestReplayReproducesTakeoverAdjudication(t *testing.T) {
	now := time.Unix(6000, 0)
	core, err := NewCore(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	rules := []Rule{
		{
			ID:        "a-night",
			Priority:  1,
			Condition: DeviceProperty{DeviceID: "daylight", Name: "dark", Operator: OpEq, Value: 1},
			Duration:  time.Minute,
			Action:    SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 1, After: 2 * time.Minute},
		},
		{
			ID:        "b-away",
			Priority:  9,
			Condition: DeviceProperty{DeviceID: "mode", Name: "away", Operator: OpEq, Value: 1},
			Action:    SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 0},
		},
	}
	for _, rule := range rules {
		if err := core.AddRule(rule); err != nil {
			t.Fatal(err)
		}
	}
	lamp := VirtualDevice{ID: "lamp", Room: "living"}
	sensor := VirtualDevice{ID: "daylight"}
	mode := VirtualDevice{ID: "mode"}

	step := func(at time.Time, fn func()) {
		if _, err := core.Tick(at); err != nil {
			t.Fatal(err)
		}
		fn()
	}
	step(now, func() { _, _ = core.Process(lamp.Event(PoweredOn, 0, now)) })
	step(now.Add(time.Minute), func() { _, _ = core.Process(sensor.Event("dark", 1, now.Add(time.Minute))) })
	step(now.Add(2*time.Minute), func() {
		_, _ = core.BeginTakeover(Takeover{Room: "living", TTL: 5 * time.Minute}, now.Add(2*time.Minute))
	})
	step(now.Add(3*time.Minute), func() { _, _ = core.Process(mode.Event("away", 1, now.Add(3*time.Minute))) })
	step(now.Add(4*time.Minute), func() { _, _ = core.Process(lamp.Status(false, now.Add(4*time.Minute))) })
	step(now.Add(8*time.Minute), func() { _, _ = core.Process(lamp.Status(true, now.Add(8*time.Minute))) })
	step(now.Add(9*time.Minute), func() { _, _ = core.Process(sensor.Event("dark", 1, now.Add(9*time.Minute))) })
	if _, err := core.Tick(now.Add(20 * time.Minute)); err != nil {
		t.Fatal(err)
	}

	replayed, replayDecisions, err := Replay(nil, rules, core.History(), now.Add(20*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(core.Decisions(), replayDecisions) {
		t.Fatalf("replay decisions diverge:\nlive=%+v\nreplay=%+v", core.Decisions(), replayDecisions)
	}
	if !reflect.DeepEqual(core.Snapshot(), replayed.Snapshot()) {
		t.Fatal("replay snapshot diverges")
	}
}
