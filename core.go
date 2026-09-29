package automation

import (
	"fmt"
	"sync"
	"time"
)

const maxEvaluationPasses = 64

type ruleRuntime struct {
	Satisfied bool      `json:"satisfied"`
	Since     time.Time `json:"since"`
	Fired     bool      `json:"fired"`
	Blocked   bool      `json:"blocked"`
}

type persistentState struct {
	Devices map[string]DeviceState `json:"devices"`
	Rules   map[string]ruleRuntime `json:"rules"`
	Actions map[string]Action      `json:"actions"`
	Seq     uint64                 `json:"seq"`
}

type Core struct {
	mu      sync.Mutex
	now     func() time.Time
	store   Store
	rules   map[string]Rule
	devices map[string]DeviceState
	runtime map[string]ruleRuntime
	pending map[string]Action
	applied []ActionRecord
	seq     uint64
}

func NewCore(store Store, now func() time.Time) (*Core, error) {
	if now == nil {
		now = time.Now
	}
	core := &Core{
		now:     now,
		store:   store,
		rules:   make(map[string]Rule),
		devices: make(map[string]DeviceState),
		runtime: make(map[string]ruleRuntime),
		pending: make(map[string]Action),
	}
	if store == nil {
		return core, nil
	}
	state, err := store.Load()
	if err != nil {
		return nil, err
	}
	if state != nil {
		core.devices = state.Devices
		core.runtime = state.Rules
		core.pending = state.Actions
		core.seq = state.Seq
		if core.devices == nil {
			core.devices = make(map[string]DeviceState)
		}
		if core.runtime == nil {
			core.runtime = make(map[string]ruleRuntime)
		}
		if core.pending == nil {
			core.pending = make(map[string]Action)
		}
	}
	return core, nil
}

func (c *Core) AddRule(rule Rule) error {
	if rule.ID == "" || rule.Condition == nil || rule.Action.DeviceID == "" || rule.Action.Name == "" {
		return fmt.Errorf("rule requires id, condition and set-state action")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.rules[rule.ID]; exists {
		return fmt.Errorf("rule %q already exists", rule.ID)
	}
	c.rules[rule.ID] = rule
	if _, exists := c.runtime[rule.ID]; !exists {
		c.runtime[rule.ID] = ruleRuntime{}
	}
	return c.saveLocked()
}

func (c *Core) Process(event Event) ([]Action, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	at := event.Time
	if at.IsZero() {
		at = c.now()
	}
	c.applyEventLocked(event, at)

	var applied []Action
	for pass := 0; pass < maxEvaluationPasses; pass++ {
		changed := false
		for _, rule := range c.orderedRulesLocked() {
			runtime := c.runtime[rule.ID]
			matched := rule.Condition.Evaluate(c.snapshotLocked())
			if !matched {
				if runtime.Satisfied || runtime.Fired || runtime.Blocked {
					c.runtime[rule.ID] = ruleRuntime{}
					changed = true
				}
				continue
			}
			if runtime.Blocked {
				continue
			}
			if !runtime.Satisfied {
				runtime.Satisfied = true
				runtime.Since = at
				runtime.Fired = false
				c.runtime[rule.ID] = runtime
				changed = true
			}
			if runtime.Fired || at.Sub(runtime.Since) < rule.Duration {
				continue
			}
			runtime.Fired = true
			c.runtime[rule.ID] = runtime
			action := c.enqueueLocked(rule, at)
			if action.CompleteAt.After(at) {
				changed = true
				continue
			}
			c.applyPendingLocked(action, at)
			applied = append(applied, action)
			changed = true
		}
		if !changed {
			break
		}
	}
	if err := c.saveLocked(); err != nil {
		return nil, err
	}
	return applied, nil
}

func (c *Core) Tick(at time.Time) ([]Action, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var applied []Action
	for pass := 0; pass < maxEvaluationPasses; pass++ {
		changed := false
		for _, action := range c.orderedPendingLocked() {
			if action.CompleteAt.After(at) {
				continue
			}
			c.applyPendingLocked(action, at)
			applied = append(applied, action)
			changed = true
		}
		for _, rule := range c.orderedRulesLocked() {
			runtime := c.runtime[rule.ID]
			matched := rule.Condition.Evaluate(c.snapshotLocked())
			if !matched {
				if runtime.Satisfied || runtime.Fired || runtime.Blocked {
					c.runtime[rule.ID] = ruleRuntime{}
					changed = true
				}
				continue
			}
			runtime = c.runtime[rule.ID]
			if runtime.Blocked || runtime.Fired {
				continue
			}
			if !runtime.Satisfied {
				runtime.Satisfied = true
				runtime.Since = at
				c.runtime[rule.ID] = runtime
				changed = true
				continue
			}
			if at.Sub(runtime.Since) < rule.Duration {
				continue
			}
			runtime.Fired = true
			c.runtime[rule.ID] = runtime
			action := c.enqueueLocked(rule, at)
			if action.CompleteAt.After(at) {
				changed = true
				continue
			}
			c.applyPendingLocked(action, at)
			applied = append(applied, action)
			changed = true
		}
		if !changed {
			break
		}
	}
	if err := c.saveLocked(); err != nil {
		return nil, err
	}
	return applied, nil
}

func (c *Core) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

func (c *Core) PendingActions() []Action {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.orderedPendingLocked()
}

func (c *Core) AppliedActions() []ActionRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ActionRecord(nil), c.applied...)
}

func (c *Core) applyEventLocked(event Event, at time.Time) {
	device := c.devices[event.DeviceID]
	device.ID = event.DeviceID
	if len(event.DeviceType) > 0 {
		device.Types = append([]string(nil), event.DeviceType...)
	}
	if device.Values == nil {
		device.Values = make(map[string]ValueState)
	}
	device.Values[event.Name] = ValueState{Value: event.Value, LastSource: event.Source}
	c.devices[event.DeviceID] = device

	if event.Source != SourceManual {
		return
	}
	for id, action := range c.pending {
		if action.DeviceID == event.DeviceID && action.Name == event.Name {
			delete(c.pending, id)
		}
	}
	for id, rule := range c.rules {
		if rule.Action.DeviceID == event.DeviceID && rule.Action.Name == event.Name {
			runtime := c.runtime[id]
			runtime.Blocked = true
			runtime.Fired = false
			c.runtime[id] = runtime
		}
	}
}

func (c *Core) enqueueLocked(rule Rule, at time.Time) Action {
	c.seq++
	action := Action{
		ID:         fmt.Sprintf("%s-%d", rule.ID, c.seq),
		RuleID:     rule.ID,
		DeviceID:   rule.Action.DeviceID,
		Name:       rule.Action.Name,
		Value:      rule.Action.Value,
		CompleteAt: at.Add(rule.Action.After),
	}
	c.pending[action.ID] = action
	return action
}

func (c *Core) applyPendingLocked(action Action, at time.Time) {
	delete(c.pending, action.ID)
	device := c.devices[action.DeviceID]
	device.ID = action.DeviceID
	if device.Values == nil {
		device.Values = make(map[string]ValueState)
	}
	device.Values[action.Name] = ValueState{Value: action.Value, LastSource: SourceAutomation}
	c.devices[action.DeviceID] = device
	c.applied = append(c.applied, ActionRecord{Action: action, At: at})
}

func (c *Core) snapshotLocked() Snapshot {
	devices := make(map[string]DeviceState, len(c.devices))
	for id, device := range c.devices {
		copyDevice := device
		copyDevice.Types = append([]string(nil), device.Types...)
		copyDevice.Values = make(map[string]ValueState, len(device.Values))
		for name, value := range device.Values {
			copyDevice.Values[name] = value
		}
		devices[id] = copyDevice
	}
	return Snapshot{Devices: devices}
}

func (c *Core) orderedRulesLocked() []Rule {
	rules := make([]Rule, 0, len(c.rules))
	for _, rule := range c.rules {
		rules = append(rules, rule)
	}
	sortRules(rules)
	return rules
}

func (c *Core) orderedPendingLocked() []Action {
	actions := make([]Action, 0, len(c.pending))
	for _, action := range c.pending {
		actions = append(actions, action)
	}
	sortActions(actions)
	return actions
}

func (c *Core) saveLocked() error {
	if c.store == nil {
		return nil
	}
	return c.store.Save(persistentState{
		Devices: c.devices,
		Rules:   c.runtime,
		Actions: c.pending,
		Seq:     c.seq,
	})
}
