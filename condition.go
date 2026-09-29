package automation

type Operator string

const (
	OpEq Operator = "eq"
	OpNe Operator = "ne"
	OpLt Operator = "lt"
	OpLe Operator = "le"
	OpGt Operator = "gt"
	OpGe Operator = "ge"
)

type Condition interface {
	Evaluate(Snapshot) bool
}

type DeviceProperty struct {
	DeviceID string
	Name     string
	Operator Operator
	Value    float64
}

func (c DeviceProperty) Evaluate(snapshot Snapshot) bool {
	value, ok := snapshot.Value(c.DeviceID, c.Name)
	return ok && compare(value, c.Operator, c.Value)
}

type AnyDeviceProperty struct {
	DeviceTypes []string
	Name        string
	Operator    Operator
	Value       float64
}

func (c AnyDeviceProperty) Evaluate(snapshot Snapshot) bool {
	for _, device := range snapshot.Devices {
		if len(c.DeviceTypes) > 0 && !snapshot.hasType(device, c.DeviceTypes) {
			continue
		}
		state, ok := device.Values[c.Name]
		if ok && compare(state.Value, c.Operator, c.Value) {
			return true
		}
	}
	return false
}

type All struct {
	Conditions []Condition
}

func (c All) Evaluate(snapshot Snapshot) bool {
	for _, condition := range c.Conditions {
		if !condition.Evaluate(snapshot) {
			return false
		}
	}
	return true
}

type Any struct {
	Conditions []Condition
}

func (c Any) Evaluate(snapshot Snapshot) bool {
	for _, condition := range c.Conditions {
		if condition.Evaluate(snapshot) {
			return true
		}
	}
	return false
}

type Not struct {
	Condition Condition
}

func (c Not) Evaluate(snapshot Snapshot) bool {
	return !c.Condition.Evaluate(snapshot)
}

func compare(left float64, operator Operator, right float64) bool {
	switch operator {
	case OpEq:
		return left == right
	case OpNe:
		return left != right
	case OpLt:
		return left < right
	case OpLe:
		return left <= right
	case OpGt:
		return left > right
	case OpGe:
		return left >= right
	default:
		return false
	}
}
