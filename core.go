package automation

import (
	"fmt"
	"sync"
	"time"
)

const maxEvaluationPasses = 64

type ruleRuntime struct {
	Satisfied    bool      `json:"satisfied"`
	Since        time.Time `json:"since"`
	Fired        bool      `json:"fired"`
	Blocked      bool      `json:"blocked"`
	BlockedUntil time.Time `json:"blocked_until,omitempty"`
}

type persistentState struct {
	Devices   map[string]DeviceState `json:"devices"`
	Rules     map[string]ruleRuntime `json:"rules"`
	Actions   map[string]Action      `json:"actions"`
	Events    []Event                `json:"events,omitempty"`
	Decisions []Decision             `json:"decisions,omitempty"`
	Seq       uint64                 `json:"seq"`
}

type Core struct {
	mu        sync.Mutex
	now       func() time.Time
	store     Store
	rules     map[string]Rule
	devices   map[string]DeviceState
	runtime   map[string]ruleRuntime
	pending   map[string]Action
	applied   []ActionRecord
	decisions []Decision
	history   []Event
	seq       uint64
}

func NewCore(store Store, now func() time.Time) (*Core, error) {
	if now == nil {
		now = time.Now
	}
	core := &Core{now: now, store: store, rules: make(map[string]Rule), devices: make(map[string]DeviceState), runtime: make(map[string]ruleRuntime), pending: make(map[string]Action)}
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
		core.history = append([]Event(nil), state.Events...)
		core.decisions = append([]Decision(nil), state.Decisions...)
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
	c.history = append(c.history, event)
	return c.advanceLocked(event, at)
}

func (c *Core) Tick(at time.Time) ([]Action, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at.IsZero() {
		at = c.now()
	}
	c.history = append(c.history, Event{Source: Source("tick"), Time: at})
	return c.advanceLocked(Event{}, at)
}

func (c *Core) Replay(events []Event) ([]Decision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	start := len(c.decisions)
	for _, event := range events {
		at := event.Time
		if at.IsZero() {
			at = c.now()
		}
		c.history = append(c.history, event)
		if _, err := c.advanceLocked(event, at); err != nil {
			return append([]Decision(nil), c.decisions[start:]...), err
		}
	}
	if err := c.saveLocked(); err != nil {
		return nil, err
	}
	return append([]Decision(nil), c.decisions[start:]...), nil
}

func (c *Core) advanceLocked(event Event, at time.Time) ([]Action, error) {
	if event.DeviceID != "" {
		c.applyEventLocked(event, at)
	}
	var applied []Action
	for pass := 0; pass < maxEvaluationPasses; pass++ {
		rulesChanged := c.evaluateRulesLocked(at)
		appliedNow, _ := c.applyDueActionsLocked(at)
		applied = append(applied, appliedNow...)
		if !rulesChanged && len(appliedNow) == 0 {
			break
		}
	}
	if err := c.saveLocked(); err != nil {
		return nil, err
	}
	return applied, nil
}

func (c *Core) Snapshot() Snapshot { c.mu.Lock(); defer c.mu.Unlock(); return c.snapshotLocked() }
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
func (c *Core) Decisions() []Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Decision(nil), c.decisions...)
}
func (c *Core) History() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.history...)
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
			c.recordDecisionLocked(action, DecisionCanceled, "superseded by manual control", at)
		}
	}
	for id, rule := range c.rules {
		if rule.Action.DeviceID == event.DeviceID && rule.Action.Name == event.Name {
			c.runtime[id] = ruleRuntime{Blocked: true, BlockedUntil: at.Add(rule.ManualOverride)}
		}
	}
}

func (c *Core) evaluateRulesLocked(at time.Time) bool {
	c.expireManualOverridesLocked(at)
	changed := false
	snapshot := c.snapshotLocked()
	for _, rule := range c.orderedRulesLocked() {
		runtime := c.runtime[rule.ID]
		matched := rule.Condition.Evaluate(snapshot)
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
			runtime.Satisfied, runtime.Since, runtime.Fired = true, at, false
			c.runtime[rule.ID] = runtime
			changed = true
			continue
		}
		if runtime.Fired || at.Sub(runtime.Since) < rule.Duration || c.hasPendingRuleLocked(rule.ID) {
			continue
		}
		runtime.Fired = true
		c.runtime[rule.ID] = runtime
		c.enqueueLocked(rule, at)
		changed = true
	}
	return changed
}

func (c *Core) applyDueActionsLocked(at time.Time) ([]Action, bool) {
	c.expireManualOverridesLocked(at)
	due := make([]Action, 0)
	waiting := false
	for _, action := range c.orderedPendingLocked() {
		if action.CompleteAt.After(at) {
			continue
		}
		if reason := c.staleActionReasonLocked(action, at); reason != "" {
			delete(c.pending, action.ID)
			c.recordDecisionLocked(action, DecisionCanceled, reason, at)
			waiting = true
			continue
		}
		if winner := c.futureConflictWinnerLocked(action); winner.ID != "" {
			waiting = true
			continue
		}
		if winner := c.activeConflictWinnerLocked(action, at); winner.ID != "" {
			delete(c.pending, action.ID)
			c.recordDecisionLocked(action, DecisionCanceled, "superseded by currently satisfied higher-priority rule "+winner.ID, at)
			waiting = true
			continue
		}
		due = append(due, action)
	}
	winnerByTarget := make(map[string]Action)
	for _, action := range due {
		target := action.DeviceID + "\x00" + action.Name
		if current, ok := winnerByTarget[target]; !ok || c.actionWinsLocked(action, current) {
			winnerByTarget[target] = action
		}
	}
	var applied []Action
	for _, action := range due {
		target := action.DeviceID + "\x00" + action.Name
		if winner := winnerByTarget[target]; winner.ID != action.ID {
			delete(c.pending, action.ID)
			c.recordDecisionLocked(action, DecisionCanceled, "superseded by higher-priority action "+winner.ID, at)
			continue
		}
		c.applyPendingLocked(action, at)
		applied = append(applied, action)
	}
	return applied, waiting || len(applied) > 0
}

func (c *Core) staleActionReasonLocked(action Action, at time.Time) string {
	rule, exists := c.rules[action.RuleID]
	if !exists {
		return "rule removed before delayed action became due"
	}
	runtime := c.runtime[rule.ID]
	if runtime.Blocked {
		return "manual override still active"
	}
	if !runtime.Fired || !runtime.Satisfied {
		return "automation condition did not remain continuously satisfied"
	}
	createdAt := action.CreatedAt
	if createdAt.IsZero() {
		createdAt = action.CompleteAt.Add(-rule.Action.After)
	}
	if runtime.Since.After(createdAt) {
		return "automation condition restarted after the action was scheduled"
	}
	if !rule.Condition.Evaluate(c.snapshotLocked()) {
		return "automation condition was no longer satisfied when the delayed action became due"
	}
	if at.After(action.CompleteAt.Add(actionValidity(rule))) {
		return "delayed action had expired"
	}
	return ""
}

func actionValidity(rule Rule) time.Duration {
	if rule.Action.After > 0 {
		return rule.Action.After
	}
	return time.Second
}

func (c *Core) futureConflictWinnerLocked(action Action) Action {
	var winner Action
	for _, candidate := range c.pending {
		if candidate.DeviceID == action.DeviceID && candidate.Name == action.Name && candidate.ID != action.ID {
			if c.actionWinsLocked(candidate, action) && (winner.ID == "" || c.actionWinsLocked(candidate, winner)) {
				winner = candidate
			}
		}
	}
	return winner
}

func (c *Core) activeConflictWinnerLocked(action Action, at time.Time) Rule {
	actionRule := c.rules[action.RuleID]
	snapshot := c.snapshotLocked()
	var winner Rule
	haveWinner := false
	for _, rule := range c.orderedRulesLocked() {
		if rule.Action.DeviceID != action.DeviceID || rule.Action.Name != action.Name || rule.Action.Value == action.Value {
			continue
		}
		runtime := c.runtime[rule.ID]
		if runtime.Blocked || !runtime.Satisfied || at.Sub(runtime.Since) < rule.Duration || !rule.Condition.Evaluate(snapshot) {
			continue
		}
		if !haveWinner || ruleWins(rule, winner) {
			winner = rule
			haveWinner = true
		}
	}
	if haveWinner && ruleWins(winner, actionRule) {
		return winner
	}
	return Rule{}
}

func ruleWins(left, right Rule) bool {
	if left.Priority != right.Priority {
		return left.Priority > right.Priority
	}
	return left.ID < right.ID
}

func (c *Core) actionWinsLocked(left, right Action) bool {
	leftRule := c.rules[left.RuleID]
	rightRule := c.rules[right.RuleID]
	if leftRule.Priority != rightRule.Priority {
		return leftRule.Priority > rightRule.Priority
	}
	if !left.CompleteAt.Equal(right.CompleteAt) {
		return left.CompleteAt.Before(right.CompleteAt)
	}
	return left.RuleID < right.RuleID
}

func (c *Core) expireManualOverridesLocked(at time.Time) {
	for id, runtime := range c.runtime {
		if runtime.Blocked && !runtime.BlockedUntil.IsZero() && !runtime.BlockedUntil.After(at) && c.rules[id].ManualOverride > 0 {
			c.runtime[id] = ruleRuntime{Satisfied: true, Since: at}
		}
	}
}

func (c *Core) hasPendingRuleLocked(ruleID string) bool {
	for _, action := range c.pending {
		if action.RuleID == ruleID {
			return true
		}
	}
	return false
}

func (c *Core) enqueueLocked(rule Rule, at time.Time) Action {
	c.seq++
	action := Action{ID: fmt.Sprintf("%s-%d", rule.ID, c.seq), RuleID: rule.ID, DeviceID: rule.Action.DeviceID, Name: rule.Action.Name, Value: rule.Action.Value, CreatedAt: at, CompleteAt: at.Add(rule.Action.After)}
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
	c.recordDecisionLocked(action, DecisionExecuted, "condition remained satisfied and action won target arbitration", at)
}

func (c *Core) recordDecisionLocked(action Action, status DecisionStatus, reason string, at time.Time) {
	c.decisions = append(c.decisions, Decision{Action: action, Status: status, Reason: reason, At: at})
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
	return c.store.Save(persistentState{Devices: c.devices, Rules: c.runtime, Actions: c.pending, Events: c.history, Decisions: c.decisions, Seq: c.seq})
}
