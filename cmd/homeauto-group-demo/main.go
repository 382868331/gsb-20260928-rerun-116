// homeauto-group-demo 演示动作组（多步骤序列）语义：
// 虚拟时钟上交错推进 准备阶段人工接管 → 部分完成后取消 → 撤销接管后
// 按当前观察重判 → 共享设备冲突裁决 → 进程重启后继续核对 → 离线设备
// 步骤不确定，并逐条打印每个效果来自哪次规则运行与哪层人工覆盖。
package main

import (
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"automation"
)

func main() {
	statePath := flag.String("state", filepath.Join("demo-runtime-group", "state.json"), "持久化状态文件路径（同一文件可模拟重启）")
	replay := flag.Bool("replay", false, "忽略持久化状态，从旧事件历史确定性重放演示场景")
	flag.Parse()

	rules := demoRules()
	clock := time.Date(2026, 9, 30, 21, 0, 0, 0, time.UTC)

	var core *automation.Core
	var err error
	if *replay {
		history, herr := readHistory(filepath.Join(filepath.Dir(*statePath), "history.json"))
		if herr != nil {
			fatal(herr)
		}
		core, _, err = automation.Replay(automation.FileStore{Path: *statePath}, rules, history, time.Time{})
		if err != nil {
			fatal(err)
		}
		fmt.Println("== 旧历史重放（不依赖实时时钟）==")
	} else {
		core, err = runScenario(*statePath, rules, clock)
		if err != nil {
			fatal(err)
		}
		if err := writeHistory(filepath.Join(filepath.Dir(*statePath), "history.json"), core.History()); err != nil {
			fatal(err)
		}
	}

	printDecisions(core.Decisions())
	printGroupRuns(core.GroupRuns())
	printSnapshot(core)
}

// demoRules 构造动作组规则：先确认窗户已关闭，再开启空调并要求设备确认。
func demoRules() []automation.Rule {
	return []automation.Rule{
		{
			ID:          "bedtime-ac",
			Description: "卧室 occupied 且温度>26℃ 持续 2 分钟：确认窗关后开空调，设备上报确认才算完成",
			Condition: automation.All{Conditions: []automation.Condition{
				automation.DeviceProperty{DeviceID: "thermostat", Name: automation.TemperatureC, Operator: automation.OpGt, Value: 26},
				automation.DeviceProperty{DeviceID: "bedroom", Name: "occupied", Operator: automation.OpEq, Value: 1},
			}},
			Duration: 2 * time.Minute,
			Priority: 5,
			Steps: []automation.Step{
				{ID: "confirm-window", Verify: &automation.StepVerify{
					DeviceID: "window", Name: automation.ContactOpen,
					Operator: automation.OpEq, Value: 0, Within: 4 * time.Minute,
				}},
				{ID: "ac-on", Action: &automation.SetStateAction{
					DeviceID: "ac", Name: automation.PoweredOn, Value: 1, After: time.Minute,
				}, ConfirmWithin: 3 * time.Minute},
			},
		},
		{
			ID:          "eco-off",
			Description: "eco 模式关闭空调，优先级高于 bedtime-ac（共享设备冲突演示）",
			Condition:   automation.DeviceProperty{DeviceID: "mode", Name: "eco", Operator: automation.OpEq, Value: 1},
			Priority:    9,
			Steps: []automation.Step{
				{ID: "ac-off", Action: &automation.SetStateAction{DeviceID: "ac", Name: automation.PoweredOn, Value: 0, After: time.Minute}},
			},
		},
		{
			ID:          "indicator-on",
			Description: "eco 模式点亮指示灯（独立设备，与 ac 冲突无关，照常推进）",
			Condition:   automation.DeviceProperty{DeviceID: "mode", Name: "eco", Operator: automation.OpEq, Value: 1},
			Steps: []automation.Step{
				{ID: "lamp", Action: &automation.SetStateAction{DeviceID: "indicator", Name: automation.PoweredOn, Value: 1}},
			},
		},
		{
			ID:          "night-humidify",
			Description: "夜间开启加湿器并要求设备确认（离线设备演示）",
			Condition:   automation.DeviceProperty{DeviceID: "bedroom", Name: "occupied", Operator: automation.OpEq, Value: 1},
			Duration:    6 * time.Minute,
			Steps: []automation.Step{
				{ID: "humidifier-on", Action: &automation.SetStateAction{
					DeviceID: "humidifier", Name: automation.PoweredOn, Value: 1,
				}, ConfirmWithin: 2 * time.Minute},
			},
		},
	}
}

func runScenario(statePath string, rules []automation.Rule, clock time.Time) (*automation.Core, error) {
	store := automation.FileStore{Path: statePath}
	core, err := automation.NewCore(store, func() time.Time { return clock })
	if err != nil {
		return nil, err
	}
	for _, rule := range rules {
		if err := core.AddRule(rule); err != nil {
			return nil, err
		}
	}
	at := func(minutes int) time.Time { return clock.Add(time.Duration(minutes) * time.Minute) }
	thermostat := automation.VirtualDevice{ID: "thermostat", Types: []string{"temperature-sensor"}}
	bedroom := automation.VirtualDevice{ID: "bedroom", Room: "bedroom"}
	window := automation.VirtualDevice{ID: "window", Room: "bedroom"}
	ac := automation.VirtualDevice{ID: "ac", Room: "bedroom"}
	mode := automation.VirtualDevice{ID: "mode"}
	humidifier := automation.VirtualDevice{ID: "humidifier", Room: "bedroom"}

	step := func(format string, args ...interface{}) {
		fmt.Printf("\n-- "+format+" --\n", args...)
	}

	step("t+00m 温度 28℃、卧室有人、窗未关、加湿器在线")
	if _, err := core.Process(thermostat.Temperature(28, at(0))); err != nil {
		return nil, err
	}
	if _, err := core.Process(bedroom.Event("occupied", 1, at(0))); err != nil {
		return nil, err
	}
	if _, err := core.Process(window.Contact(true, at(0))); err != nil {
		return nil, err
	}
	if _, err := core.Process(humidifier.Availability(true, at(0))); err != nil {
		return nil, err
	}

	step("t+02m 条件持续满足：bedtime-ac 启动，进入准备阶段（等待窗关确认）")
	if _, err := core.Tick(at(2)); err != nil {
		return nil, err
	}

	step("t+03m 准备阶段被人工接管（guest 接管卧室）：等待中的确认与后续动作停止")
	if err := core.Takeover(automation.Override{ID: "guest",
		Scope: automation.OverrideScope{Room: "bedroom"}, Reason: "客人暂住"}, at(3)); err != nil {
		return nil, err
	}

	step("t+04m 接管期内关窗：不得推进任何步骤")
	if _, err := core.Process(window.Contact(false, at(4))); err != nil {
		return nil, err
	}
	if _, err := core.Tick(at(4)); err != nil {
		return nil, err
	}

	step("t+05m 撤销接管：规则从头重新观察条件，旧动作不补发")
	if err := core.RevokeOverride("guest", at(5)); err != nil {
		return nil, err
	}
	if _, err := core.Tick(at(5)); err != nil {
		return nil, err
	}

	step("t+07m 条件重新持续满足：新运行按当前观察判断，窗已关直接确认，重新排定开空调")
	if _, err := core.Tick(at(7)); err != nil {
		return nil, err
	}

	step("t+07m 加湿器离线：指向它的动作到点时将取消并记录 device_offline")
	if _, err := core.Process(humidifier.Availability(false, at(7))); err != nil {
		return nil, err
	}

	step("t+07m30s eco 模式开启：共享 ac 发生冲突，eco-off 优先级更高；独立设备 indicator 照常推进")
	if _, err := core.Process(mode.Event("eco", 1, at(7).Add(30*time.Second))); err != nil {
		return nil, err
	}

	step("t+08m bedtime-ac 的空调动作到点前被更高优先级的 eco-off 取代（决策记录冲突依据）")
	if _, err := core.Tick(at(8)); err != nil {
		return nil, err
	}

	step("t+09m eco-off 的空调动作落地（ac=0）")
	if _, err := core.Tick(at(9)); err != nil {
		return nil, err
	}

	step("t+10m eco 模式结束；bedtime-ac 第三次运行排定开空调")
	if _, err := core.Process(mode.Event("eco", 0, at(10))); err != nil {
		return nil, err
	}
	if _, err := core.Tick(at(10)); err != nil {
		return nil, err
	}

	step("== 进程重启：从状态文件恢复，继续核对未完成的确认 ==")
	restarted, err := automation.NewCore(store, func() time.Time { return clock })
	if err != nil {
		return nil, err
	}
	for _, rule := range rules {
		if err := restarted.AddRule(rule); err != nil {
			return nil, err
		}
	}
	core = restarted

	step("t+12m 重启后空调动作按原截止时刻（t+11m）落地；加湿器动作因设备离线取消")
	if _, err := core.Tick(at(12)); err != nil {
		return nil, err
	}

	step("t+12m30s 空调以上报确认已开启：整组此刻才记为完成")
	if _, err := core.Process(ac.Event(automation.PoweredOn, 1, at(12).Add(30*time.Second))); err != nil {
		return nil, err
	}
	if _, err := core.Tick(at(13)); err != nil {
		return nil, err
	}
	return core, nil
}

func printDecisions(decisions []automation.Decision) {
	fmt.Println("\n== 决策日志（每个效果可追溯：规则 / 运行 / 步骤 / 接管层）==")
	for _, d := range decisions {
		line := fmt.Sprintf("%s  %-9s %-24s rule=%s", d.At.Format("15:04"), d.Outcome, d.Reason, d.RuleID)
		if d.RunID != "" {
			line += " run=" + d.RunID
		}
		if d.StepID != "" {
			line += " step=" + d.StepID
		}
		if d.OverrideID != "" {
			line += " override=" + d.OverrideID
		}
		if d.DeviceID != "" {
			line += fmt.Sprintf(" %s.%s", d.DeviceID, d.Name)
		}
		if d.Detail != "" {
			line += "  # " + d.Detail
		}
		fmt.Println(line)
	}
}

func printGroupRuns(runs []automation.GroupRun) {
	fmt.Println("\n== 动作组最终状态 ==")
	for _, run := range runs {
		fmt.Printf("%s (%s) done=%v\n", run.RuleID, run.RunID, run.Done)
		for _, s := range run.Steps {
			fmt.Printf("  step %-16s %s\n", s.StepID, s.Status)
		}
	}
}

func printSnapshot(core *automation.Core) {
	fmt.Println("\n== 最终设备状态 ==")
	snapshot := core.Snapshot()
	for _, id := range []string{"window", "ac", "indicator", "humidifier"} {
		device, ok := snapshot.Devices[id]
		if !ok {
			continue
		}
		for name, value := range device.Values {
			fmt.Printf("%s.%s = %v (last source: %s)\n", id, name, value.Value, value.LastSource)
		}
	}
}

func fatal(err error) {
	fmt.Println("demo failed:", err)
	panic(err)
}
