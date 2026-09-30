// homeauto-demo 是家居自动化核心的独立可执行演示：
// 不依赖任何真实硬件，用虚拟设备事件驱动一个确定性的夜间场景，
// 交错覆盖 夜间持续条件 → 延迟开灯 → 临时手动关闭 → 进程重启 →
// 覆盖到期 → 冲突裁决 → 相互触发但不循环，并逐条打印动作为何执行或取消。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"automation"
)

func main() {
	statePath := flag.String("state", filepath.Join("demo-runtime", "state.json"), "持久化状态文件路径（同一文件可模拟重启）")
	replay := flag.Bool("replay", false, "忽略持久化状态，从旧事件历史确定性重放演示场景")
	flag.Parse()

	rules := demoRules()
	clock := time.Date(2026, 9, 30, 18, 0, 0, 0, time.UTC)

	var core *automation.Core
	var decisions []automation.Decision
	var err error
	if *replay {
		history := loadHistory(filepath.Join(filepath.Dir(*statePath), "history.jsonl"))
		core, decisions, err = automation.Replay(automation.FileStore{Path: *statePath}, rules, history, clock.Add(40*time.Minute))
		if err != nil {
			fatal(err)
		}
		fmt.Println("== 旧历史重放（不依赖实时时钟）==")
	} else {
		core, err = automation.NewCore(automation.FileStore{Path: *statePath}, func() time.Time { return clock })
		if err != nil {
			fatal(err)
		}
		for _, rule := range rules {
			if err := core.AddRule(rule); err != nil {
				fatal(err)
			}
		}
		core, err = runScenario(core, *statePath, clock)
		if err != nil {
			fatal(err)
		}
		decisions = core.Decisions()
	}

	printDecisions(decisions)
	printSnapshot(core)
}

// demoRules 构造一组相互交错、可验证全部语义的规则。
func demoRules() []automation.Rule {
	return []automation.Rule{
		{
			ID:          "night-light",
			Description: "夜间（天黑持续 5 分钟）后延迟 3 分钟开灯；手动关灯覆盖 8 分钟",
			Condition:   automation.DeviceProperty{DeviceID: "daylight", Name: "dark", Operator: automation.OpEq, Value: 1},
			Duration:    5 * time.Minute,
			ManualTTL:   8 * time.Minute,
			Priority:    5,
			Action:      automation.SetStateAction{DeviceID: "lamp", Name: automation.PoweredOn, Value: 1, After: 3 * time.Minute},
		},
		{
			ID:          "movie-dim",
			Description: "影院模式开始即关灯，优先级高于夜间开灯",
			Condition:   automation.DeviceProperty{DeviceID: "remote", Name: "movie_mode", Operator: automation.OpEq, Value: 1},
			Priority:    10,
			Action:      automation.SetStateAction{DeviceID: "lamp", Name: automation.PoweredOn, Value: 0},
		},
		{
			ID:          "lamp-indicator",
			Description: "灯亮则点亮指示灯（由自动化产生的状态触发，只触发一次，不循环）",
			Condition: automation.All{Conditions: []automation.Condition{
				automation.DeviceProperty{DeviceID: "lamp", Name: automation.PoweredOn, Operator: automation.OpEq, Value: 1},
				automation.DeviceProperty{DeviceID: "daylight", Name: "dark", Operator: automation.OpEq, Value: 1},
			}},
			Action: automation.SetStateAction{DeviceID: "indicator", Name: automation.PoweredOn, Value: 1},
		},
	}
}

type journal struct {
	file *os.File
}

func openJournal(path string) (*journal, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &journal{file: f}, nil
}

func (j *journal) append(event automation.Event) {
	data, _ := json.Marshal(event)
	_, _ = j.file.Write(append(data, '\n'))
}

func loadHistory(path string) []automation.Event {
	data, err := os.ReadFile(path)
	if err != nil {
		fatal(fmt.Errorf("读取历史 %s：%w（请先不带 --replay 运行演示）", path, err))
	}
	var events []automation.Event
	for _, line := range splitLines(data) {
		if len(line) == 0 {
			continue
		}
		var event automation.Event
		if err := json.Unmarshal(line, &event); err != nil {
			fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

// runScenario 以固定虚拟时钟推进场景。进程重启通过用同一状态文件
// 重新构造 Core 来真实模拟，而不是在内存里直接继续。
func runScenario(core *automation.Core, statePath string, start time.Time) (*automation.Core, error) {
	j, err := openJournal(filepath.Join(filepath.Dir(statePath), "history.jsonl"))
	if err != nil {
		return nil, err
	}
	defer j.file.Close()

	daylight := automation.VirtualDevice{ID: "daylight", Types: []string{"light-sensor"}}
	remote := automation.VirtualDevice{ID: "remote", Types: []string{"scene-remote"}}
	at := func(minute int) time.Time { return start.Add(time.Duration(minute) * time.Minute) }

	var stepErr error
	step := func(when time.Time, label string, event *automation.Event) {
		fmt.Printf("[t+%02dm] %s\n", int(when.Sub(start).Minutes()), label)
		if event != nil {
			j.append(*event)
			if _, err := core.Process(*event); err != nil {
				stepErr = err
				return
			}
		} else {
			if _, err := core.Tick(when); err != nil {
				stepErr = err
				return
			}
		}
		printState(core)
	}

	event := func(e automation.Event) *automation.Event { return &e }

	step(at(0), "天色开始变暗（夜间条件出现）", event(daylight.Event("dark", 1, at(0))))
	step(at(3), "仍然天黑：持续时间未满 5 分钟，不动作", nil)
	step(at(5), "持续满 5 分钟：排定 3 分钟后开灯（延迟动作）", nil)
	step(at(7), "用户手动关灯：挂起动作立即取消，规则进入 8 分钟人工覆盖", event(automation.ManualControl("lamp", automation.PoweredOn, 0, at(7))))
	step(at(9), "覆盖期内推进时间：自动化不得抢回控制权", nil)
	if stepErr != nil {
		return nil, stepErr
	}

	// 真实重启：同一状态文件构造新核，规则重新注册后继续。
	fmt.Printf("[t+10m] 模拟进程重启（从 %s 恢复）\n", statePath)
	restarted, err := automation.NewCore(automation.FileStore{Path: statePath}, func() time.Time { return at(10) })
	if err != nil {
		return nil, err
	}
	for _, rule := range demoRules() {
		if err := restarted.AddRule(rule); err != nil {
			return nil, err
		}
	}
	core = restarted
	step(at(10), "重启完成：设备状态、覆盖期限、待执行动作均已恢复", nil)

	step(at(14), "人工覆盖即将到期，但旧动作早已取消，不会补执行", nil)
	step(at(15), "覆盖到期：重新开始观察夜间条件（需再次持续 5 分钟）", nil)
	step(at(16), "重新计时中：不动作", nil)
	step(at(20), "再次持续满 5 分钟：重新排定延迟开灯", nil)
	step(at(23), "延迟到点且夜间条件仍成立：开灯并联动指示灯", nil)
	step(at(25), "按下影院模式：高优先级规则裁决关灯，夜间动作被取代", event(remote.Event("movie_mode", 1, at(25))))
	if stepErr != nil {
		return nil, stepErr
	}
	return core, nil
}

func printState(core *automation.Core) {
	snapshot := core.Snapshot()
	for _, id := range []string{"lamp", "indicator"} {
		value, ok := snapshot.Value(id, automation.PoweredOn)
		if ok {
			fmt.Printf("         %s=%v\n", id, value)
		}
	}
}

func printDecisions(decisions []automation.Decision) {
	fmt.Println()
	fmt.Println("== 决策日志（每个动作为何执行/取消）==")
	if len(decisions) == 0 {
		fmt.Println("（无）")
		return
	}
	for _, d := range decisions {
		value := "-"
		if d.Value != nil {
			value = fmt.Sprintf("%v", *d.Value)
		}
		fmt.Printf("%s  %-9s %-28s rule=%-15s action=%-18s %s.%s -> %s\n",
			d.At.Format("15:04:05"), d.Outcome, d.Reason, d.RuleID, d.ActionID, d.DeviceID, d.Name, value)
	}
}

func printSnapshot(core *automation.Core) {
	data, _ := json.MarshalIndent(core.Snapshot(), "", "  ")
	fmt.Println()
	fmt.Println("== 最终设备快照 ==")
	fmt.Println(string(data))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}
