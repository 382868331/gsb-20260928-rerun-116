package smarthome

import (
	"testing"
)

func boolProp(t *testing.T, e *Engine, dev, prop string) bool {
	t.Helper()
	v, ok := e.Device(dev).Get(prop)
	if !ok {
		t.Fatalf("%s.%s missing", dev, prop)
	}
	b, _ := v.Boolean()
	return b
}

func numProp(t *testing.T, e *Engine, dev, prop string) float64 {
	t.Helper()
	v, ok := e.Device(dev).Get(prop)
	if !ok {
		t.Fatalf("%s.%s missing", dev, prop)
	}
	f, _ := v.Float()
	return f
}

// 温度短暂越界不应触发要求持续满足的规则；持续满足才触发。
func TestSustainRequired(t *testing.T) {
	bus := &Bus{}
	clk := NewFakeClock()
	e := NewEngine(bus, clk)
	RegisterEnv(e, 28)
	for _, r := range DefaultRules() {
		if err := e.AddRule(r); err != nil {
			t.Fatal(err)
		}
	}
	env := e.Device(EnvDevice).(*VirtualDevice)

	// 越界 3s（要求持续 5s），随后恢复：不应触发。
	env.Update("temp", Number(31))
	clk.Advance(3 * Second)
	e.Tick(clk.Now())
	env.Update("temp", Number(28))
	clk.Advance(1 * Second)
	e.Tick(clk.Now())
	if got := e.Stats().Triggered; got != 0 {
		t.Fatalf("brief spike triggered rule, triggered=%d", got)
	}
	if boolProp(t, e, CoolerDevice, "on") {
		t.Fatal("cooler turned on by brief spike")
	}

	// 再次越界并持续满 5s：触发第一步（开空调）。
	env.Update("temp", Number(31))
	clk.Advance(5 * Second)
	e.Tick(clk.Now())
	if !boolProp(t, e, CoolerDevice, "on") {
		t.Fatal("cooler should be on after sustained heat")
	}
	// 条件继续成立，不能每个 Tick 重复触发。
	clk.Advance(5 * Second)
	e.Tick(clk.Now())
	if got := e.Stats().Triggered; got != 1 {
		t.Fatalf("rule re-triggered while condition held: %d", got)
	}
}

// 手动接管后，自动动作不能立刻覆盖；解除接管后，下一个真周期才能动作。
func TestManualOverride(t *testing.T) {
	bus := &Bus{}
	clk := NewFakeClock()
	e := NewEngine(bus, clk)
	RegisterEnv(e, 32)
	rules := DefaultRules()
	if err := e.AddRule(rules[0]); err != nil {
		t.Fatal(err)
	}

	// 高温计时到第 4s 时，人手动关掉空调并接管。
	clk.Advance(4 * Second)
	e.Tick(clk.Now())
	if err := e.ManualSet(CoolerDevice, "on", Bool(false)); err != nil {
		t.Fatal(err)
	}
	// 到第 6s 规则触发，开空调动作必须被接管锁挡下。
	clk.Advance(2 * Second)
	e.Tick(clk.Now())
	if boolProp(t, e, CoolerDevice, "on") {
		t.Fatal("auto action overrode manual control immediately")
	}
	if got := e.Stats().Suppressed; got == 0 {
		t.Fatal("expected suppressed action step")
	}

	// 条件一直为真，解除接管后规则也不会立刻补动作（边沿语义）。
	e.ReleaseOverride(CoolerDevice, "on")
	clk.Advance(2 * Second)
	e.Tick(clk.Now())
	if boolProp(t, e, CoolerDevice, "on") {
		t.Fatal("rule fired without a new false->true cycle")
	}

	// 温度回落再升高并持续，形成新的真周期，此时规则可以动作。
	env := e.Device(EnvDevice).(*VirtualDevice)
	env.Update("temp", Number(28))
	clk.Advance(1 * Second)
	e.Tick(clk.Now())
	env.Update("temp", Number(32))
	clk.Advance(5 * Second)
	e.Tick(clk.Now())
	if !boolProp(t, e, CoolerDevice, "on") {
		t.Fatal("rule should fire in a new true cycle after release")
	}
}

// 规则引起的新状态不能形成无限触发环：构造一个振荡环，验证它被因果链切断。
func TestNoInfiniteLoop(t *testing.T) {
	bus := &Bus{}
	clk := NewFakeClock()
	e := NewEngine(bus, clk)
	RegisterEnv(e, 25)

	// 用一个独立的数值开关设备构造 4 条规则的环：
	// A(=0)->1, B(=1)->2, C(=2)->3, D(=3)->0，均为即时规则。
	loop := NewVirtualDevice("loop", bus).
		Init("x", Number(0), true)
	e.AddDevice(loop)
	mk := func(id string, from, to float64) *Rule {
		return &Rule{
			ID:         id,
			All:        []Condition{{Device: "loop", Prop: "x", Op: OpEq, Value: Number(from)}},
			SustainFor: 0,
			Action: Action{ID: id + "-act", Steps: []Step{
				{Delay: 0, Device: "loop", Prop: "x", Value: Number(to)},
			}},
		}
	}
	for _, r := range []*Rule{mk("a", 0, 1), mk("b", 1, 2), mk("c", 2, 3), mk("d", 3, 0)} {
		if err := e.AddRule(r); err != nil {
			t.Fatal(err)
		}
	}

	// 系统启动一个新的因果链：若没有防环，这里会无限递归。
	loop.Update("x", Number(0))
	e.Tick(clk.Now())

	// 沿链 a->b->c->d，d 再触发 a 时发现 a 已在链上，环被切断，x 停在 0。
	// 被切断的最后一步不会写回，因此稳定状态为 d 写入的 0。
	if got := numProp(t, e, "loop", "x"); got != 0 {
		t.Fatalf("loop did not settle, x=%v", got)
	}
	if got := e.Stats().Blocked; got == 0 {
		t.Fatal("expected at least one blocked cascading step")
	}
}

// 保存状态后重启：持续计时与未完成动作继续，并用互相影响的两条规则验证。
func TestRestartResumesTimersAndPending(t *testing.T) {
	path := t.TempDir() + "/state.json"
	bus := &Bus{}
	clk := NewFakeClock()
	e := NewEngine(bus, clk)
	RegisterEnv(e, 32)
	for _, r := range DefaultRules() {
		if err := e.AddRule(r); err != nil {
			t.Fatal(err)
		}
	}

	// 高温持续 3s 后保存重启：5s 的持续计时尚未完成。
	clk.Advance(3 * Second)
	e.Tick(clk.Now())
	if err := e.SaveToFile(path); err != nil {
		t.Fatal(err)
	}

	// 新引擎、新时钟从同一时刻继续。
	bus2 := &Bus{}
	clk2 := NewFakeClock()
	clk2.Advance(3 * Second)
	e2 := NewEngine(bus2, clk2)
	RegisterEnv(e2, 32)
	for _, r := range DefaultRules() {
		if err := e2.AddRule(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := e2.LoadFromFile(path); err != nil {
		t.Fatal(err)
	}
	if got := numProp(t, e2, EnvDevice, "temp"); got != 32 {
		t.Fatalf("temp not restored: %v", got)
	}

	// 再走 2s（累计高温 5s）：规则触发，空调打开。
	clk2.Advance(2 * Second)
	e2.Tick(clk2.Now())
	if !boolProp(t, e2, CoolerDevice, "on") {
		t.Fatal("cooler should turn on after resumed timer completes")
	}
	if boolProp(t, e2, WindowDevice, "open") {
		t.Fatal("window step (due 10s later) must still be pending")
	}

	// 开窗步骤在触发后 10s 到期；在它到期前再次重启，验证“未完成动作”继续。
	clk2.Advance(4 * Second)
	e2.Tick(clk2.Now())
	if err := e2.SaveToFile(path); err != nil {
		t.Fatal(err)
	}
	bus3 := &Bus{}
	clk3 := NewFakeClock()
	clk3.Advance(9 * Second) // 开窗步骤在绝对时间 15s 到期，此刻还差 6s
	e3 := NewEngine(bus3, clk3)
	RegisterEnv(e3, 32)
	for _, r := range DefaultRules() {
		if err := e3.AddRule(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := e3.LoadFromFile(path); err != nil {
		t.Fatal(err)
	}
	if !boolProp(t, e3, CoolerDevice, "on") {
		t.Fatal("cooler state should survive restart")
	}
	if boolProp(t, e3, WindowDevice, "open") {
		t.Fatal("window should remain closed before pending step is due")
	}

	// 到触发后 10s（绝对时间 15s），待执行步骤落地：开窗。
	clk3.Advance(6 * Second)
	e3.Tick(clk3.Now())
	if !boolProp(t, e3, WindowDevice, "open") {
		t.Fatal("pending window step should execute after restart")
	}

	// 两条规则互相影响：开窗持续 8s 后，规则 2 关空调。
	clk3.Advance(8 * Second)
	e3.Tick(clk3.Now())
	if boolProp(t, e3, CoolerDevice, "on") {
		t.Fatal("ventilation rule should switch cooler off")
	}
}

// 手动接管锁也必须跨重启保留。
func TestOverrideSurvivesRestart(t *testing.T) {
	path := t.TempDir() + "/state.json"
	bus := &Bus{}
	clk := NewFakeClock()
	e := NewEngine(bus, clk)
	RegisterEnv(e, 32)
	if err := e.AddRule(DefaultRules()[0]); err != nil {
		t.Fatal(err)
	}
	if err := e.ManualSet(CoolerDevice, "on", Bool(false)); err != nil {
		t.Fatal(err)
	}
	if err := e.SaveToFile(path); err != nil {
		t.Fatal(err)
	}

	bus2 := &Bus{}
	clk2 := NewFakeClock()
	e2 := NewEngine(bus2, clk2)
	RegisterEnv(e2, 32)
	if err := e2.AddRule(DefaultRules()[0]); err != nil {
		t.Fatal(err)
	}
	if err := e2.LoadFromFile(path); err != nil {
		t.Fatal(err)
	}
	clk2.Advance(6 * Second)
	e2.Tick(clk2.Now())
	if boolProp(t, e2, CoolerDevice, "on") {
		t.Fatal("manual override must survive restart")
	}
}
