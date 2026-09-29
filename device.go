package smarthome

import "fmt"

// Source 标识一次状态变化的来源。事件含义（“某设备的某属性变成了某值”）
// 对所有设备保持一致：门窗、温度以及以后接入的任何设备都走同一套语义。
type Source string

const (
	// SourceManual 来自人（App/物理开关），优先级最高。
	SourceManual Source = "manual"
	// SourceSystem 来自设备自身/虚拟环境反馈，不携带任何自动因果链。
	SourceSystem Source = "system"
	// SourceAuto 来自规则引擎执行的动作。
	SourceAuto Source = "auto"
)

// Event 是总线上唯一的数据形态：设备/属性/旧值/新值/来源/因果链。
type Event struct {
	Device string
	Prop   string
	Old    Value
	New    Value
	Source Source
	// Trace 是本次自动级联中已经触发过动作的规则链，用于防止触发环。
	Trace []string
}

// Handler 订阅设备状态变化。
type Handler func(Event)

// Bus 是设备与引擎之间解耦的事件总线。
type Bus struct {
	handlers []Handler
}

func (b *Bus) Subscribe(h Handler) {
	b.handlers = append(b.handlers, h)
}

func (b *Bus) publish(e Event) {
	// 复制一份快照，避免处理器内部订阅/退订造成迭代问题。
	hs := append([]Handler(nil), b.handlers...)
	for _, h := range hs {
		h(e)
	}
}

// Value 支持数值、布尔与字符串三种属性值。
type Value struct {
	kind byte // 'n' number, 'b' bool, 's' string
	num  float64
	bl   bool
	str  string
}

func Number(v float64) Value { return Value{kind: 'n', num: v} }
func Bool(v bool) Value      { return Value{kind: 'b', bl: v} }
func String(v string) Value  { return Value{kind: 's', str: v} }

func (v Value) Float() (float64, bool) {
	if v.kind != 'n' {
		return 0, false
	}
	return v.num, true
}

func (v Value) Boolean() (bool, bool) {
	if v.kind != 'b' {
		return false, false
	}
	return v.bl, true
}

func (v Value) String_() (string, bool) {
	if v.kind != 's' {
		return "", false
	}
	return v.str, true
}

func (v Value) Equals(o Value) bool {
	switch v.kind {
	case 'n':
		return o.kind == 'n' && v.num == o.num
	case 'b':
		return o.kind == 'b' && v.bl == o.bl
	default:
		return o.kind == 's' && v.str == o.str
	}
}

// String 返回可读表示，用于日志与演示。
func (v Value) Display() string {
	switch v.kind {
	case 'n':
		return fmt.Sprintf("%g", v.num)
	case 'b':
		return fmt.Sprintf("%t", v.bl)
	default:
		return v.str
	}
}

// Device 是所有设备（虚拟或将来的真实设备）的统一抽象。
type Device interface {
	ID() string
	Get(prop string) (Value, bool)
	Snapshot() map[string]Value
	// Set 写属性；只读属性返回 false。是否真正下发由引擎的接管锁决定，
	// 设备本身不关心调用方是人还是规则。
	Set(prop string, v Value) bool
	Writable(prop string) bool
}

// VirtualDevice 是内存中的虚拟设备：门窗、温度传感器都用它实现。
// 只读属性（如温度）由虚拟环境更新，可写属性（如开关/开合）由人或规则更新。
type VirtualDevice struct {
	id       string
	state    map[string]Value
	writable map[string]bool
	bus      *Bus
}

func NewVirtualDevice(id string, bus *Bus) *VirtualDevice {
	return &VirtualDevice{
		id:       id,
		state:    map[string]Value{},
		writable: map[string]bool{},
		bus:      bus,
	}
}

func (d *VirtualDevice) ID() string { return d.id }

// Init 写入初始属性。writable 决定该属性能否被 Set 修改。
func (d *VirtualDevice) Init(prop string, v Value, writable bool) *VirtualDevice {
	d.state[prop] = v
	d.writable[prop] = writable
	return d
}

// Update 供虚拟环境/物理反馈更新只读属性，恒可写并发出 system 事件。
func (d *VirtualDevice) Update(prop string, v Value) bool {
	return d.change(prop, v, SourceSystem, nil)
}

func (d *VirtualDevice) Get(prop string) (Value, bool) {
	v, ok := d.state[prop]
	return v, ok
}

func (d *VirtualDevice) Snapshot() map[string]Value {
	out := make(map[string]Value, len(d.state))
	for k, v := range d.state {
		out[k] = v
	}
	return out
}

func (d *VirtualDevice) Writable(prop string) bool { return d.writable[prop] }

// Set 实现 Device：由引擎在确认接管锁后调用，事件标记为 auto。
func (d *VirtualDevice) Set(prop string, v Value) bool {
	return d.change(prop, v, SourceAuto, nil)
}

// emit 允许引擎带上来源与因果链（动作为 auto + trace）。
func (d *VirtualDevice) change(prop string, v Value, src Source, trace []string) bool {
	old, exists := d.state[prop]
	if exists && old.Equals(v) {
		return false
	}
	if !exists || src != SourceAuto || d.writable[prop] {
		d.state[prop] = v
		d.bus.publish(Event{
			Device: d.id, Prop: prop, Old: old, New: v,
			Source: src, Trace: append([]string(nil), trace...),
		})
		return true
	}
	return false
}
