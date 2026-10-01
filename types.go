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
	Room       string    `json:"room,omitempty"`
	Name       string    `json:"name"`
	Value      float64   `json:"value"`
	Source     Source    `json:"source"`
	Time       time.Time `json:"time"`
	// Takeover 仅用于接管事件（EventTakeoverBegin / EventTakeoverEnd），
	// 普通设备事件与人工控制事件为 nil。
	Takeover *Takeover `json:"takeover,omitempty"`
}

// 接管事件与设备在线状态使用的事件/属性名。
const (
	EventTakeoverBegin = "takeover_begin"
	EventTakeoverEnd   = "takeover_end"
	// Online 是设备上报在线状态使用的属性名（1 在线，0 离线）。
	Online = "online"
)

// Takeover 描述一层人工接管。多层接管可以叠加：每层有独立的作用范围
// （整个房间、整台设备或设备的某个属性）与独立的到期时间，
// 只有全部覆盖某规则的接管层都结束，该规则才恢复自动观察。
type Takeover struct {
	ID string `json:"id"`
	// Room 非空表示接管该房间内的全部设备。
	Room string `json:"room,omitempty"`
	// DeviceID 非空表示只接管该设备；Name 为空时接管整台设备，
	// 否则只接管该属性（临时例外）。
	DeviceID string `json:"device_id,omitempty"`
	Name     string `json:"name,omitempty"`
	// TTL 为接管有效时长，0 表示直到显式撤销。
	TTL time.Duration `json:"ttl,omitempty"`
	// Until 为接管截止时刻，由 BeginTakeover 按 TTL 计算，nil 表示不过期。
	Until     *time.Time `json:"until,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
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
	// OutcomeTakeoverBegin / OutcomeTakeoverEnd 记录接管层的建立与结束，
	// 使历史能解释“当时为什么自动化没有接管该设备”。
	OutcomeTakeoverBegin Outcome = "takeover_begin"
	OutcomeTakeoverEnd   Outcome = "takeover_end"
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
	ReasonTakeoverStarted    Reason = "takeover_started"
	ReasonTakeoverExpired    Reason = "takeover_expired"
	ReasonTakeoverReleased   Reason = "takeover_released"
	ReasonDeviceOffline      Reason = "device_offline"
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
	// TakeoverID 在接管相关决策中标识对应的接管层。
	TakeoverID string `json:"takeover_id,omitempty"`
	// DueAt 非空表示该动作本应在 DueAt 落地，但进程当时已停止
	// （或时钟未及时推进），实际到 At 才被处理。用于区分
	// “停机期间应当发生的决策”与“实际执行了的动作”。
	DueAt *time.Time `json:"due_at,omitempty"`
}
