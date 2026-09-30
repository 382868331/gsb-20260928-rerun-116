package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	automation "automation"
)

var printedDecisions = map[string]bool{}

func main() {
	statePath := filepath.Join(os.TempDir(), "smart-home-virtual-demo.json")
	_ = os.Remove(statePath)
	store := automation.FileStore{Path: statePath}
	location, _ := time.LoadLocation("Asia/Shanghai")
	start := time.Date(2026, 9, 30, 22, 0, 0, 0, location)
	outdoor := automation.VirtualDevice{ID: "outdoor", Types: []string{"environment"}}
	room := automation.VirtualDevice{ID: "living-room", Types: []string{"room"}}

	rules := []automation.Rule{
		{
			ID:             "night-lamp-on",
			Description:    "夜间持续 1 分钟后，15 分钟延迟开灯",
			Condition:      automation.DeviceProperty{DeviceID: "outdoor", Name: automation.NightDark, Operator: automation.OpEq, Value: 1},
			Duration:       time.Minute,
			Priority:       10,
			ManualOverride: 30 * time.Minute,
			Action:         automation.SetStateAction{DeviceID: "lamp", Name: automation.PoweredOn, Value: 1, After: 15 * time.Minute},
		},
		{
			ID:          "movie-lamp-off",
			Description: "夜间看电影优先关灯",
			Condition: automation.All{Conditions: []automation.Condition{
				automation.DeviceProperty{DeviceID: "outdoor", Name: automation.NightDark, Operator: automation.OpEq, Value: 1},
				automation.DeviceProperty{DeviceID: "living-room", Name: "movie_mode", Operator: automation.OpEq, Value: 1},
			}},
			Duration: time.Minute,
			Priority: 20,
			Action:   automation.SetStateAction{DeviceID: "lamp", Name: automation.PoweredOn, Value: 0, After: 15 * time.Minute},
		},
	}

	core, err := automation.NewCore(store, func() time.Time { return start })
	fail(err)
	for _, rule := range rules {
		fail(core.AddRule(rule))
	}

	process(core, outdoor.Night(true, start), "22:00 夜间开始")
	tick(core, start.Add(time.Minute), "22:01 持续条件满足，登记延迟开灯")
	process(core, automation.ManualControl("lamp", automation.PoweredOn, 0, start.Add(5*time.Minute)), "22:05 临时手动关灯")

	restarted, err := automation.NewCore(store, func() time.Time { return start.Add(20 * time.Minute) })
	fail(err)
	for _, rule := range rules {
		fail(restarted.AddRule(rule))
	}
	tick(restarted, start.Add(20*time.Minute), "22:20 重启：旧动作仍被人工覆盖取消")
	tick(restarted, start.Add(35*time.Minute), "22:35 覆盖到期：重新计时并产生新动作")
	process(restarted, room.Event("movie_mode", 1, start.Add(35*time.Minute)), "22:35 电影模式开始")
	tick(restarted, start.Add(36*time.Minute), "22:36 高优先级关灯也满足持续条件")
	process(restarted, room.Event("movie_mode", 0, start.Add(45*time.Minute)), "22:45 电影模式结束")
	tick(restarted, start.Add(51*time.Minute), "22:51 到期：冲突关灯已失效，夜间开灯执行")

	replayCore, err := automation.NewCore(nil, func() time.Time { return start })
	fail(err)
	for _, rule := range rules {
		fail(replayCore.AddRule(rule))
	}
	decisions, err := replayCore.Replay(restarted.History())
	fail(err)
	fmt.Printf("旧历史在新核心重放：%d 条裁决\n", len(decisions))
	fmt.Println("演示完成，设备状态:", replayCore.Snapshot().Devices)
	fmt.Println("持久化文件:", statePath)
}

func process(core *automation.Core, event automation.Event, label string) {
	_, err := core.Process(event)
	fail(err)
	printDecisions(core, label)
}

func tick(core *automation.Core, at time.Time, label string) {
	_, err := core.Tick(at)
	fail(err)
	printDecisions(core, label)
}

func printDecisions(core *automation.Core, label string) {
	fmt.Println("\n" + label)
	for _, action := range core.PendingActions() {
		fmt.Printf("  pending %s -> %s=%v at %s\n", action.RuleID, action.Name, action.Value, action.CompleteAt.Format("15:04:05"))
	}
	for _, decision := range core.Decisions() {
		key := fmt.Sprintf("%s-%s", decision.Action.ID, decision.Status)
		if printedDecisions[key] {
			continue
		}
		printedDecisions[key] = true
		fmt.Printf("  %s %s -> %s=%v: %s\n", decision.Status, decision.Action.RuleID, decision.Action.Name, decision.Action.Value, decision.Reason)
	}
}

func fail(err error) {
	if err != nil {
		panic(err)
	}
}
