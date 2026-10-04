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
	// Online 是设备可用性约定属性：值为 0 表示设备离线。
	// 离线设备的其他属性对条件不可见，指向它的动作到点时取消。
	Online = "online"
)

type Event struct {
	DeviceID   string    `json:"device_id"`
	DeviceType []string  `json:"device_type,omitempty"`
	Room       string    `json:"room,omitempty"`
	Name       string    `json:"name"`
	Value      float64   `json:"value"`
	Source     Source    `json:"source"`
	Time       time.Time `json:"time"`
	// Override 非空时该事件表示建立一层人工接管；RevokeID 非空表示
	// 撤销指定接管层。两类事件都进入历史，重放时可复现同一裁决。
	Override *Override `json:"override,omitempty"`
	RevokeID string    `json:"revoke_id,omitempty"`
	// Tick 为 true 表示该条目是一次时钟推进记录（无设备语义），
	// 使历史重放能在完全相同的时间点上做裁决。
	Tick bool `json:"tick,omitempty"`
}

type ValueState struct {
	Value      float64 `json:"value"`
	LastSource Source  `json:"last_source"`
}

type DeviceState struct {
	ID     string                `json:"id"`
	Types  []string              `json:"types,omitempty"`
	Room   string                `json:"room,omitempty"`
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
	if name != Online {
		if availability, ok := device.Values[Online]; ok && availability.Value == 0 {
			return 0, false
		}
	}
	state, ok := device.Values[name]
	return state.Value, ok
}

// Offline 报告指定设备当前是否离线（online 属性为 0）。
func (s Snapshot) Offline(deviceID string) bool {
	device, ok := s.Devices[deviceID]
	if !ok {
		return false
	}
	availability, ok := device.Values[Online]
	return ok && availability.Value == 0
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
	// ValidFor 限定动作到点后的有效窗口：重启或停机导致错过到点时刻，
	// 且落后超过该时长时动作视为过时，取消而不是补发。0 表示不限制。
	ValidFor time.Duration `json:"valid_for,omitempty"`
}

// StepVerify 描述一个“确认效果”步骤：等待设备属性达到目标值。
// 在 Within 内未确认时该步骤记为不确定（uncertain），后续步骤不再执行。
type StepVerify struct {
	DeviceID string
	Name     string
	Operator Operator
	Value    float64
	// Within 为确认等待窗口；0 表示只在步骤开始时检查一次。
	Within time.Duration
}

// Step 是动作组中的一个步骤：要么执行设备动作，要么等待确认某个效果。
// Action 与 Verify 必须且只能填一个。
type Step struct {
	ID     string
	Action *SetStateAction
	Verify *StepVerify
	// ConfirmWithin 仅对动作步骤有效：动作落地后等待设备以上报（SourceDevice）
	// 确认目标值的窗口；0 表示落地即视为确认。窗口内未确认则步骤记为不确定。
	ConfirmWithin time.Duration
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
	// Steps 非空时规则触发的是一组按序执行的步骤（动作组），
	// 此时 Action 字段被忽略。组内每一步独立记录目标、确认与不确定状态，
	// 整组只有在全部步骤确认后才记为完成。
	Steps []Step
}

type Action struct {
	ID         string    `json:"id"`
	RuleID     string    `json:"rule_id"`
	DeviceID   string    `json:"device_id"`
	Name       string    `json:"name"`
	Value      float64   `json:"value"`
	CompleteAt time.Time `json:"complete_at"`
	// ExpiresAt 非空时，晚于该时刻仍未落地的动作视为过时，不再补发。
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// RunID / StepID 非空时表明该动作属于某次动作组运行的某一步，
	// 用于把每个效果追溯到具体的规则判断（运行）与步骤。
	RunID  string `json:"run_id,omitempty"`
	StepID string `json:"step_id,omitempty"`
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
	// OutcomeOverride 记录人工接管层的建立、到期与撤销。
	OutcomeOverride Outcome = "override"
	// OutcomeGroup 记录动作组运行与组内步骤的生命周期决策。
	OutcomeGroup Outcome = "group"
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
	ReasonDeviceOffline      Reason = "device_offline"
	ReasonStaleAction        Reason = "stale_action_expired"
	ReasonOverrideStarted    Reason = "override_started"
	ReasonOverrideExpired    Reason = "override_expired"
	ReasonOverrideRevoked    Reason = "override_revoked"
	// 动作组相关原因。
	ReasonGroupStarted   Reason = "group_started"
	ReasonGroupCompleted Reason = "group_completed"
	ReasonGroupCanceled  Reason = "group_canceled"
	ReasonStepConfirmed  Reason = "step_confirmed"
	ReasonStepUncertain  Reason = "step_uncertain"
	// ReasonStepSkipped 表示步骤目标在当前观察下已经满足，无需再执行。
	ReasonStepSkipped Reason = "step_already_satisfied"
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
	// OverrideID 标明该决策涉及的人工接管层（如适用）。
	OverrideID string `json:"override_id,omitempty"`
	// RunID / StepID 把决策追溯到具体的一次动作组运行与步骤。
	RunID  string `json:"run_id,omitempty"`
	StepID string `json:"step_id,omitempty"`
	// Detail 补充人可读说明，例如停机期间本应发生但未补发的时刻。
	Detail string `json:"detail,omitempty"`
}
