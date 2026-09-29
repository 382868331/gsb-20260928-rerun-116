package smarthome

import "fmt"

// Op 是条件比较运算。
type Op string

const (
	OpEq Op = "=="
	OpNe Op = "!="
	OpGt Op = ">"
	OpGe Op = ">="
	OpLt Op = "<"
	OpLe Op = "<="
)

// Condition 针对任意设备的任意属性。门窗（bool）与温度（number）
// 使用同一种表达，新设备接入时规则定义无需改动。
type Condition struct {
	Device string
	Prop   string
	Op     Op
	Value  Value
}

func (c Condition) eval(devs map[string]Device) (bool, error) {
	d, ok := devs[c.Device]
	if !ok {
		return false, fmt.Errorf("condition: device %q not found", c.Device)
	}
	v, ok := d.Get(c.Prop)
	if !ok {
		return false, fmt.Errorf("condition: device %q has no prop %q", c.Device, c.Prop)
	}
	if err := typeCompatible(v, c.Value); err != nil {
		return false, fmt.Errorf("condition %s.%s: %w", c.Device, c.Prop, err)
	}
	switch c.Op {
	case OpEq:
		return v.Equals(c.Value), nil
	case OpNe:
		return !v.Equals(c.Value), nil
	}
	switch c.Value.kind {
	case 'n':
		a, _ := v.Float()
		b, _ := c.Value.Float()
		switch c.Op {
		case OpGt:
			return a > b, nil
		case OpGe:
			return a >= b, nil
		case OpLt:
			return a < b, nil
		case OpLe:
			return a <= b, nil
		}
	case 's':
		a, _ := v.String_()
		b, _ := c.Value.String_()
		switch c.Op {
		case OpGt:
			return a > b, nil
		case OpGe:
			return a >= b, nil
		case OpLt:
			return a < b, nil
		case OpLe:
			return a <= b, nil
		}
	}
	return false, fmt.Errorf("condition: op %q not supported for value type", c.Op)
}

func typeCompatible(a, b Value) error {
	if a.kind != b.kind {
		return fmt.Errorf("type mismatch %q vs %q", a.Display(), b.Display())
	}
	return nil
}

// Step 是动作序列中的一步：等待 Delay 后把属性设置为 Value。
// 支持多步是为了表达“未完成动作”——例如先关空调、延时 30s 再关新风。
type Step struct {
	Delay  Duration
	Device string
	Prop   string
	Value  Value
}

// Action 是规则触发后执行的有序步骤序列。
type Action struct {
	ID    string
	Steps []Step
}

// Rule 是一条自动化规则。
//
// All 中所有条件同时成立并持续 SustainFor 后才触发（边沿触发：
// 每个“假→真并保持”的周期最多触发一次，避免条件恒真时反复触发）。
// 触发后依次调度 Action 的各步骤，步骤之间的等待会落盘，重启不丢。
type Rule struct {
	ID         string
	Name       string
	All        []Condition
	SustainFor Duration
	Action     Action
}

func (r *Rule) satisfied(devs map[string]Device) (bool, error) {
	for i := range r.All {
		ok, err := r.All[i].eval(devs)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}
