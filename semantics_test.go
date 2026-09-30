package automation

import (
	"path/filepath"
	"testing"
	"time"
)

func TestConflictingActionsAreArbitratedByPriority(t *testing.T) {
	now := time.Unix(1000, 0)
	core, _ := NewCore(nil, func() time.Time { return now })
	sensor := VirtualDevice{ID: "room", Types: []string{"environment"}}
	rules := []Rule{
		{ID: "night-light", Priority: 10, Condition: DeviceProperty{DeviceID: "room", Name: NightDark, Operator: OpEq, Value: 1}, Action: SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 1, After: time.Minute}},
		{ID: "movie-off", Priority: 20, Condition: All{Conditions: []Condition{
			DeviceProperty{DeviceID: "room", Name: NightDark, Operator: OpEq, Value: 1},
			DeviceProperty{DeviceID: "room", Name: "movie", Operator: OpEq, Value: 1},
		}}, Action: SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 0, After: time.Minute}},
	}
	for _, rule := range rules {
		if err := core.AddRule(rule); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := core.Process(sensor.Event(NightDark, 1, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Process(sensor.Event("movie", 1, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Tick(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if value, _ := core.Snapshot().Value("lamp", PoweredOn); value != 0 {
		t.Fatalf("higher-priority off action did not win: %v", value)
	}
	decisions := core.Decisions()
	var canceled, executed int
	for _, decision := range decisions {
		switch decision.Status {
		case DecisionCanceled:
			canceled++
		case DecisionExecuted:
			executed++
		}
	}
	if canceled != 1 || executed != 1 {
		t.Fatalf("expected one canceled and one executed decision, got %+v", decisions)
	}
}

func TestDelayedActionIsCanceledWhenConditionBecomesStale(t *testing.T) {
	now := time.Unix(2000, 0)
	core, _ := NewCore(nil, func() time.Time { return now })
	sensor := VirtualDevice{ID: "outdoor", Types: []string{"environment"}}
	if err := core.AddRule(Rule{ID: "night-light", Condition: DeviceProperty{DeviceID: "outdoor", Name: NightDark, Operator: OpEq, Value: 1}, Duration: time.Minute, Action: SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 1, After: 2 * time.Minute}}); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Process(sensor.Night(true, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Tick(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(core.PendingActions()) != 1 {
		t.Fatal("expected delayed action")
	}
	if _, err := core.Process(sensor.Night(false, now.Add(2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Tick(now.Add(3 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, ok := core.Snapshot().Value("lamp", PoweredOn); ok {
		t.Fatal("stale delayed action was executed")
	}
	last := core.Decisions()[len(core.Decisions())-1]
	if last.Status != DecisionCanceled || last.Reason == "" {
		t.Fatalf("expected explained cancellation, got %+v", last)
	}
}

func TestManualOverrideExpiresWithoutReplayingOldActionAcrossRestart(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := FileStore{Path: statePath}
	now := time.Unix(3000, 0)
	core, _ := NewCore(store, func() time.Time { return now })
	sensor := VirtualDevice{ID: "outdoor", Types: []string{"environment"}}
	rule := Rule{ID: "night-light", ManualOverride: 10 * time.Minute, Condition: DeviceProperty{DeviceID: "outdoor", Name: NightDark, Operator: OpEq, Value: 1}, Action: SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 1, After: time.Minute}}
	if err := core.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Process(sensor.Night(true, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Tick(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	oldAction := core.PendingActions()[0]
	if _, err := core.Process(ManualControl("lamp", PoweredOn, 0, now.Add(2*time.Second))); err != nil {
		t.Fatal(err)
	}
	if len(core.PendingActions()) != 0 {
		t.Fatal("manual control did not remove old automation action")
	}

	restarted, _ := NewCore(store, func() time.Time { return now })
	if err := restarted.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Tick(now.Add(5 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(restarted.PendingActions()) != 0 {
		t.Fatal("automation resumed before manual override expired")
	}
	if _, err := restarted.Tick(now.Add(11 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(restarted.PendingActions()) != 1 {
		t.Fatal("override expiry should schedule a fresh action")
	}
	if restarted.PendingActions()[0].ID == oldAction.ID {
		t.Fatal("old action was replayed after override ended")
	}
	if _, err := restarted.Tick(now.Add(12 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if value, _ := restarted.Snapshot().Value("lamp", PoweredOn); value != 1 {
		t.Fatal("fresh post-override action did not execute")
	}
}

func TestReplayOldHistoryProducesSameDecisionLog(t *testing.T) {
	now := time.Unix(4000, 0)
	original, _ := NewCore(nil, func() time.Time { return now })
	sensor := VirtualDevice{ID: "outdoor", Types: []string{"environment"}}
	rule := Rule{ID: "night-light", Condition: DeviceProperty{DeviceID: "outdoor", Name: NightDark, Operator: OpEq, Value: 1}, Duration: time.Minute, Action: SetStateAction{DeviceID: "lamp", Name: PoweredOn, Value: 1}}
	if err := original.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	events := []Event{sensor.Night(true, now), sensor.Night(false, now.Add(30*time.Second)), sensor.Night(true, now.Add(time.Minute))}
	if _, err := original.Replay(events); err != nil {
		t.Fatal(err)
	}
	if _, err := original.Tick(now.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}

	replayed, _ := NewCore(nil, func() time.Time { return now })
	if err := replayed.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	if _, err := replayed.Replay(original.History()); err != nil {
		t.Fatal(err)
	}
	if _, err := replayed.Tick(now.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(replayed.Decisions()) != len(original.Decisions()) {
		t.Fatalf("decision count changed: %d != %d", len(replayed.Decisions()), len(original.Decisions()))
	}
}
