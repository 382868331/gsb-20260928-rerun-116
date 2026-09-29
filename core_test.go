package automation

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDurationRequiresContinuousCondition(t *testing.T) {
	now := time.Unix(100, 0)
	core, err := NewCore(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sensor := VirtualDevice{ID: "thermostat", Types: []string{"temperature-sensor"}}
	if err := core.AddRule(Rule{
		ID:        "cooling",
		Condition: DeviceProperty{DeviceID: "thermostat", Name: TemperatureC, Operator: OpGt, Value: 30},
		Duration:  time.Minute,
		Action:    SetStateAction{DeviceID: "ac", Name: PoweredOn, Value: 1},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := core.Process(sensor.Temperature(31, now)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(59 * time.Second)
	if _, err := core.Process(sensor.Temperature(32, now)); err != nil {
		t.Fatal(err)
	}
	if value, ok := core.Snapshot().Value("ac", PoweredOn); ok && value == 1 {
		t.Fatalf("temperature spike triggered action: %v", value)
	}

	now = now.Add(time.Second)
	if _, err := core.Process(sensor.Temperature(25, now)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := core.Process(sensor.Temperature(31, now)); err != nil {
		t.Fatal(err)
	}
	if _, ok := core.Snapshot().Value("ac", PoweredOn); ok {
		t.Fatal("reset condition still used the old duration timer")
	}

	now = now.Add(time.Minute)
	actions, err := core.Process(sensor.Temperature(31, now))
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected one action after sustained condition, got %d", len(actions))
	}
	if value, ok := core.Snapshot().Value("ac", PoweredOn); !ok || value != 1 {
		t.Fatalf("ac not powered on: value=%v ok=%v", value, ok)
	}
}

func TestManualControlBlocksAutomationUntilRearm(t *testing.T) {
	now := time.Unix(200, 0)
	core, err := NewCore(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sensor := VirtualDevice{ID: "thermostat", Types: []string{"temperature-sensor"}}
	rule := Rule{
		ID:        "cooling",
		Condition: DeviceProperty{DeviceID: "thermostat", Name: TemperatureC, Operator: OpGt, Value: 30},
		Duration:  time.Minute,
		Action:    SetStateAction{DeviceID: "ac", Name: PoweredOn, Value: 1, After: 30 * time.Second},
	}
	if err := core.AddRule(rule); err != nil {
		t.Fatal(err)
	}

	if _, err := core.Process(sensor.Temperature(31, now)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := core.Process(sensor.Temperature(31, now)); err != nil {
		t.Fatal(err)
	}
	if len(core.PendingActions()) != 1 {
		t.Fatal("expected delayed automatic action")
	}

	if _, err := core.Process(ManualControl("ac", PoweredOn, 0, now.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	if len(core.PendingActions()) != 0 {
		t.Fatal("manual control did not cancel pending automation")
	}
	if value, _ := core.Snapshot().Value("ac", PoweredOn); value != 0 {
		t.Fatalf("automation overrode manual control: %v", value)
	}

	if _, err := core.Process(sensor.Temperature(25, now.Add(time.Hour+time.Second))); err != nil {
		t.Fatal(err)
	}
	rearmedAt := now.Add(time.Hour + 2*time.Second)
	if _, err := core.Process(sensor.Temperature(31, rearmedAt)); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Tick(rearmedAt.Add(time.Minute + time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(core.PendingActions()) != 1 {
		t.Fatal("expected rearmed automatic action to wait for its delay")
	}
	actions, err := core.Tick(rearmedAt.Add(2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected rearmed action, got %+v", actions)
	}
	if value, _ := core.Snapshot().Value("ac", PoweredOn); value != 1 {
		t.Fatalf("rule did not rearm after condition became false: %v", value)
	}
}

func TestInteractingRulesEvaluateOnceAndDoNotLoop(t *testing.T) {
	now := time.Unix(300, 0)
	core, err := NewCore(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sensor := VirtualDevice{ID: "thermostat", Types: []string{"temperature-sensor"}}

	rules := []Rule{
		{
			ID:        "open-window",
			Condition: DeviceProperty{DeviceID: "thermostat", Name: TemperatureC, Operator: OpGt, Value: 30},
			Duration:  time.Minute,
			Action:    SetStateAction{DeviceID: "window", Name: ContactOpen, Value: 1},
		},
		{
			ID: "stop-cooling",
			Condition: All{Conditions: []Condition{
				DeviceProperty{DeviceID: "thermostat", Name: TemperatureC, Operator: OpGt, Value: 30},
				DeviceProperty{DeviceID: "window", Name: ContactOpen, Operator: OpEq, Value: 1},
			}},
			Duration: time.Minute,
			Action:   SetStateAction{DeviceID: "ac", Name: PoweredOn, Value: 0},
		},
	}
	for _, rule := range rules {
		if err := core.AddRule(rule); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := core.Process(sensor.Temperature(31, now)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	actions, err := core.Process(sensor.Temperature(31, now))
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].DeviceID != "window" {
		t.Fatalf("only first rule should fire at its threshold: %+v", actions)
	}
	now = now.Add(time.Minute)
	actions, err = core.Process(sensor.Temperature(31, now))
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].DeviceID != "ac" {
		t.Fatalf("second rule should fire once: %+v", actions)
	}
	if len(core.AppliedActions()) != 2 {
		t.Fatalf("rule-generated state created a trigger loop: %+v", core.AppliedActions())
	}
}

func TestRestartContinuesDurationAndDelayedActionChain(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	now := time.Unix(400, 0)
	store := FileStore{Path: statePath}
	core, err := NewCore(store, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sensor := VirtualDevice{ID: "thermostat", Types: []string{"temperature-sensor"}}
	rules := []Rule{
		{
			ID:        "fan",
			Condition: DeviceProperty{DeviceID: "thermostat", Name: TemperatureC, Operator: OpGt, Value: 30},
			Duration:  time.Minute,
			Action:    SetStateAction{DeviceID: "fan", Name: PoweredOn, Value: 1, After: time.Minute},
		},
		{
			ID:        "indicator",
			Condition: DeviceProperty{DeviceID: "fan", Name: PoweredOn, Operator: OpEq, Value: 1},
			Action:    SetStateAction{DeviceID: "indicator", Name: PoweredOn, Value: 1},
		},
	}
	for _, rule := range rules {
		if err := core.AddRule(rule); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := core.Process(sensor.Temperature(31, now)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := core.Process(sensor.Temperature(31, now)); err != nil {
		t.Fatal(err)
	}
	if len(core.PendingActions()) != 1 {
		t.Fatal("expected unfinished fan action before restart")
	}

	now = now.Add(30 * time.Second)
	restarted, err := NewCore(store, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range rules {
		if err := restarted.AddRule(rule); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := restarted.Tick(now); err != nil {
		t.Fatal(err)
	}
	if _, ok := restarted.Snapshot().Value("fan", PoweredOn); ok {
		t.Fatal("delayed action completed before its persisted deadline")
	}

	now = now.Add(31 * time.Second)
	actions, err := restarted.Tick(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 {
		t.Fatalf("expected delayed fan action and dependent indicator, got %+v", actions)
	}
	if value, _ := restarted.Snapshot().Value("fan", PoweredOn); value != 1 {
		t.Fatal("fan did not continue after restart")
	}
	if value, _ := restarted.Snapshot().Value("indicator", PoweredOn); value != 1 {
		t.Fatal("dependent rule did not continue from action-generated state")
	}
}
