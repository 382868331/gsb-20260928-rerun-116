// 演示：虚拟门窗/温度设备 + 两条互相影响的规则，真实时钟驱动并周期性落盘。
// 运行：go run ./cmd/demo
package main

import (
	"fmt"
	"os"
	"time"

	sh "smarthome"
)

func main() {
	statePath := "demo-state.json"
	bus := &sh.Bus{}
	clock := sh.NewWallClock()
	engine := sh.NewEngine(bus, clock)

	var env *sh.Env
	if _, err := os.Stat(statePath); err == nil {
		// 重启路径：先重建设备/规则，再加载快照，计时与未完成动作继续。
		env = sh.RegisterEnv(engine, 20)
		for _, r := range sh.DefaultRules() {
			if err := engine.AddRule(r); err != nil {
				panic(err)
			}
		}
		if err := engine.LoadFromFile(statePath); err != nil {
			panic(err)
		}
		env = sh.RestoreEnv(engine)
		fmt.Println("[boot] 状态已从快照恢复")
	} else {
		env = sh.RegisterEnv(engine, 33) // 开局高温
		for _, r := range sh.DefaultRules() {
			if err := engine.AddRule(r); err != nil {
				panic(err)
			}
		}
		fmt.Println("[boot] 全新启动，初始室温 33℃")
	}

	bus.Subscribe(func(ev sh.Event) {
		src := map[sh.Source]string{
			sh.SourceManual: "人", sh.SourceSystem: "环境", sh.SourceAuto: "规则",
		}[ev.Source]
		fmt.Printf("  事件 %-7s %s.%s: %s -> %s\n", src, ev.Device, ev.Prop, ev.Old.Display(), ev.New.Display())
	})

	tick := 250 * time.Millisecond
	t := time.NewTicker(tick)
	defer t.Stop()
	saver := time.NewTicker(time.Second)
	defer saver.Stop()
	for range (30 * time.Second) / tick {
		select {
		case <-t.C:
			now := clock.Now()
			env.Step(now)
			engine.Tick(now)
		case <-saver.C:
			if err := engine.SaveToFile(statePath); err != nil {
				fmt.Println("save error:", err)
			}
		}
	}
	s := engine.Stats()
	fmt.Printf("[done] 触发=%d 被接管挡下=%d 被防环/非法挡下=%d 最终室温=%.1f℃\n",
		s.Triggered, s.Suppressed, s.Blocked, env.Temp())
}
