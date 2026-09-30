package automation

import "time"

type Source string

const (
	SourceDevice     Source = "device"
	SourceManual     Source = "manual"
	SourceAutomation Source = "automation"
)

const (
	ContactOpen  = "contact_open"
	TemperatureC = "temperature_c"
	PoweredOn    = "powered_on"
)

type Event struct {
	DeviceID   string    `json:"device_id"`
	DeviceType []string  `json:"device_type,omitempty"`
	Name       string    `json:"name"`
	Value      float64   `json:"value"`
	Source     Source    `json:"source"`
	Time       time.Time `json:"time"`
}

type ValueState struct {
	Value      float64 `json:"value"`
	LastSource Source  `json:"last_source"`
}

type DeviceState struct {
	ID     string                `json:"id"`
	Types  []string              `json:"types,omitempty"`
	Values map[string]ValueState `json:"values"`
}

type Snapshot struct {
	Devices map[string]DeviceState
}

func (s Snapshot) Value(deviceID, name string) (float64, bool) {
	device, ok := s.Devices[deviceID]
	if !ok {
		return 0, false
	}
	state, ok := device.Values[name]
	return state.Value, ok
}

func (s Snapshot) hasType(device DeviceState, wanted []string) bool {
	for _, typ := range wanted {
		for _, actual := range device.Types {
			if actual == typ {
				return true
			}
		}
	}
	return false
}

type SetStateAction struct {
	DeviceID string        `json:"device_id"`
	Name     string        `json:"name"`
	Value    float64       `json:"value"`
	After    time.Duration `json:"after"`
}

type Rule struct {
	ID          string
	Description string
	Condition   Condition
	Duration    time.Duration
	// Priority 决定同一设备同一属性上互相冲突动作的裁决结果，数值越大越优先；
	// 相同优先级时规则 ID 较小者稳定获胜。
	Priority int
	// ManualTTL 为人工覆盖的有效期限：
	//   0 表示覆盖持续到条件重新变假并再次为真（默认）；
	//   大于 0 表示从人工操作时刻起经过该时长后自动恢复。
	ManualTTL time.Duration
	Action    SetStateAction
}

type Action struct {
	ID         string    `json:"id"`
	RuleID     string    `json:"rule_id"`
	DeviceID   string    `json:"device_id"`
	Name       string    `json:"name"`
	Value      float64   `json:"value"`
	CompleteAt time.Time `json:"complete_at"`
}

type ActionRecord struct {
	Action Action
	At     time.Time
}

// Outcome 描述一次动作裁决的最终去向。
type Outcome string

const (
	OutcomeExecuted  Outcome = "executed"
	OutcomeCanceled  Outcome = "canceled"
	OutcomeScheduled Outcome = "scheduled"
)

// Reason 给出动作被执行或取消的确定原因，便于逐条核对。
type Reason string

const (
	ReasonConditionHeld      Reason = "condition_held"
	ReasonConditionFalse     Reason = "condition_false"
	ReasonRuleBlocked        Reason = "rule_blocked_manual"
	ReasonSuperseded         Reason = "superseded_by_higher_priority"
	ReasonDuplicate          Reason = "duplicate_pending"
	ReasonManualIntervention Reason = "manual_intervention"
)

// Decision 记录每个动作为什么被执行、延迟或取消，构成确定性日志。
type Decision struct {
	At       time.Time `json:"at"`
	Outcome  Outcome   `json:"outcome"`
	Reason   Reason    `json:"reason"`
	RuleID   string    `json:"rule_id"`
	ActionID string    `json:"action_id,omitempty"`
	DeviceID string    `json:"device_id,omitempty"`
	Name     string    `json:"name,omitempty"`
	Value    *float64  `json:"value,omitempty"`
}
