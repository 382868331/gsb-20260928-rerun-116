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
	NightDark    = "night_dark"
)

type Event struct {
	DeviceID   string
	DeviceType []string
	Name       string
	Value      float64
	Source     Source
	Time       time.Time
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
	ID             string
	Description    string
	Condition      Condition
	Duration       time.Duration
	Action         SetStateAction
	Priority       int
	ManualOverride time.Duration
}

type Action struct {
	ID         string    `json:"id"`
	RuleID     string    `json:"rule_id"`
	DeviceID   string    `json:"device_id"`
	Name       string    `json:"name"`
	Value      float64   `json:"value"`
	CreatedAt  time.Time `json:"created_at"`
	CompleteAt time.Time `json:"complete_at"`
}

type ActionRecord struct {
	Action Action
	At     time.Time
}

type DecisionStatus string

const (
	DecisionExecuted DecisionStatus = "executed"
	DecisionCanceled DecisionStatus = "canceled"
)

type Decision struct {
	Action Action         `json:"action"`
	Status DecisionStatus `json:"status"`
	Reason string         `json:"reason"`
	At     time.Time      `json:"at"`
}
