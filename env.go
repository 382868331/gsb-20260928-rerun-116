package smarthome

// Env 是虚拟物理环境：把设备状态映射成温度变化，让规则之间真实地互相影响。
//
// 模型（每经过 dt 毫秒）：
//   - 开着窗：室内温度每 2s 下降 1℃（向外界散热）；
//   - 空调制冷开启：每 1s 下降 1℃；
//   - 否则温度缓慢回升（每 5s 上升 1℃，上限 35℃）。
//
// 温度传感器是只读属性，只能由本环境更新；门窗/空调是可写属性。
type Env struct {
	env    *VirtualDevice // temp 只读
	window *VirtualDevice // open 可写
	cooler *VirtualDevice // on 可写
	door   *VirtualDevice // open 可写
	last   Duration
}

const (
	EnvDevice    = "env"
	WindowDevice = "window"
	CoolerDevice = "cooler"
	DoorDevice   = "door"
)

// RegisterEnv 创建并注册四个虚拟设备，返回环境句柄。
func RegisterEnv(e *Engine, startTemp float64) *Env {
	bus := e.bus
	env := NewVirtualDevice(EnvDevice, bus).
		Init("temp", Number(startTemp), false)
	window := NewVirtualDevice(WindowDevice, bus).
		Init("open", Bool(false), true)
	cooler := NewVirtualDevice(CoolerDevice, bus).
		Init("on", Bool(false), true)
	door := NewVirtualDevice(DoorDevice, bus).
		Init("open", Bool(false), true)
	e.AddDevice(env)
	e.AddDevice(window)
	e.AddDevice(cooler)
	e.AddDevice(door)
	return &Env{env: env, window: window, cooler: cooler, door: door, last: e.clock.Now()}
}

// RestoreEnv 在从快照恢复时使用：设备已注册，仅重建环境句柄。
func RestoreEnv(e *Engine) *Env {
	return &Env{
		env:    e.devices[EnvDevice].(*VirtualDevice),
		window: e.devices[WindowDevice].(*VirtualDevice),
		cooler: e.devices[CoolerDevice].(*VirtualDevice),
		door:   e.devices[DoorDevice].(*VirtualDevice),
		last:   e.clock.Now(),
	}
}

func (x *Env) Temp() float64 {
	v, _ := x.env.Get("temp")
	f, _ := v.Float()
	return f
}

// Step 根据设备当前状态推进物理时间并更新温度。
func (x *Env) Step(now Duration) {
	dt := now - x.last
	x.last = now
	if dt <= 0 {
		return
	}
	windowOpen, _ := getBool(x.window, "open")
	coolerOn, _ := getBool(x.cooler, "on")
	temp := x.Temp()
	var delta float64
	switch {
	case coolerOn:
		delta = -float64(dt) / float64(Second)
	case windowOpen:
		delta = -float64(dt) / (2 * float64(Second))
	default:
		delta = float64(dt) / (5 * float64(Second))
	}
	temp += delta
	if temp > 35 {
		temp = 35
	}
	if temp < 5 {
		temp = 5
	}
	// Update 发出 system 事件，驱动引擎重新评估持续条件。
	x.env.Update("temp", Number(temp))
}

func getBool(d *VirtualDevice, prop string) (bool, bool) {
	v, ok := d.Get(prop)
	if !ok {
		return false, false
	}
	return v.Boolean()
}

// DefaultRules 返回两条会互相影响的规则：
//  1. 高温持续 5s -> 先开空调；再过 10s 若仍高温则开窗（多步、跨重启的未完成动作）。
//  2. 门窗任意一个打开持续 8s -> 关空调（开窗通风时不必制冷），这会使温度回升，
//     进而影响规则 1 —— 两条规则通过虚拟环境真实地互相作用。
func DefaultRules() []*Rule {
	hot := Condition{Device: EnvDevice, Prop: "temp", Op: OpGt, Value: Number(30)}
	return []*Rule{
		{
			ID:         "cool-when-hot",
			Name:       "持续高温先开空调，仍热则开窗",
			All:        []Condition{hot},
			SustainFor: 5 * Second,
			Action: Action{ID: "cool-down", Steps: []Step{
				{Delay: 0, Device: CoolerDevice, Prop: "on", Value: Bool(true)},
				{Delay: 10 * Second, Device: WindowDevice, Prop: "open", Value: Bool(true)},
			}},
		},
		{
			ID:   "close-ac-with-open-air",
			Name: "门窗持续打开则关空调",
			All: []Condition{
				{Device: WindowDevice, Prop: "open", Op: OpEq, Value: Bool(true)},
			},
			SustainFor: 8 * Second,
			Action: Action{ID: "ventilate", Steps: []Step{
				{Delay: 0, Device: CoolerDevice, Prop: "on", Value: Bool(false)},
			}},
		},
	}
}
