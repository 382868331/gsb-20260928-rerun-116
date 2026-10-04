package automation

import (
	"fmt"
	"sync"
	"time"
)

const maxEvaluationPasses = 64

type ruleRuntime struct {
	Satisfied    bool       `json:"satisfied"`
	Since        time.Time  `json:"since"`
	Fired        bool       `json:"fired"`
	Blocked      bool       `json:"blocked"`
	BlockedUntil *time.Time `json:"blocked_until,omitempty"`
	// Group 是动作组规则当前/最近一次运行的状态，随核一起持久化，
	// 重启后据此继续核对未完成的步骤，而不是补发旧动作。
	Group *groupRuntime `json:"group,omitempty"`
}

type persistentState struct {
	Devices   map[string]DeviceState `json:"devices"`
	Rules     map[string]ruleRuntime `json:"rules"`
	Actions   map[string]Action      `json:"actions"`
	Overrides map[string]Override    `json:"overrides,omitempty"`
	Seq       uint64                 `json:"seq"`
	History   []Event                `json:"history,omitempty"`
	Applied   []ActionRecord         `json:"applied,omitempty"`
	Decisions []Decision             `json:"decisions,omitempty"`
}

type Core struct {
	mu        sync.Mutex
	now       func() time.Time
	store     Store
	rules     map[string]Rule
	devices   map[string]DeviceState
	runtime   map[string]ruleRuntime
	pending   map[string]Action
	overrides map[string]Override
	applied   []ActionRecord
	history   []Event
	decisions []Decision
	seq       uint64
}

func NewCore(store Store, now func() time.Time) (*Core, error) {
	if now == nil {
		now = time.Now
	}
	core := &Core{
		now:       now,
		store:     store,
		rules:     make(map[string]Rule),
		devices:   make(map[string]DeviceState),
		runtime:   make(map[string]ruleRuntime),
		pending:   make(map[string]Action),
		overrides: make(map[string]Override),
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
		core.overrides = state.Overrides
		core.seq = state.Seq
		core.applied = state.Applied
		core.history = state.History
		core.decisions = state.Decisions
		if core.devices == nil {
			core.devices = make(map[string]DeviceState)
		}
		if core.runtime == nil {
			core.runtime = make(map[string]ruleRuntime)
		}
		if core.pending == nil {
			core.pending = make(map[string]Action)
		}
		if core.overrides == nil {
			core.overrides = make(map[string]Override)
		}
	}
	return core, nil
}

func (c *Core) AddRule(rule Rule) error {
	if rule.ID == "" || rule.Condition == nil {
		return fmt.Errorf("rule requires id, condition and set-state action")
	}
	if len(rule.Steps) == 0 && (rule.Action.DeviceID == "" || rule.Action.Name == "") {
		return fmt.Errorf("rule requires id, condition and set-state action")
	}
	for i, step := range rule.Steps {
		if step.ID == "" {
			return fmt.Errorf("rule %q step %d requires id", rule.ID, i)
		}
		if (step.Action == nil) == (step.Verify == nil) {
			return fmt.Errorf("rule %q step %q requires exactly one of action or verify", rule.ID, step.ID)
		}
		if step.Action != nil && (step.Action.DeviceID == "" || step.Action.Name == "") {
			return fmt.Errorf("rule %q step %q action requires device and name", rule.ID, step.ID)
		}
		if step.Verify != nil && (step.Verify.DeviceID == "" || step.Verify.Name == "") {
			return fmt.Errorf("rule %q step %q verify requires device and name", rule.ID, step.ID)
		}
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
	event.Time = at
	c.history = append(c.history, event)
	c.applyEventLocked(event, at)

	applied := c.evaluateLocked(at)
	if err := c.saveLocked(); err != nil {
		return nil, err
	}
	return applied, nil
}

func (c *Core) Tick(at time.Time) ([]Action, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 把时钟推进也记入历史：重放仅在事件时刻推进时钟，
	// 记录 Tick 后虚拟时间回放才能复现完全相同的裁决时点。
	c.history = append(c.history, Event{Time: at, Tick: true})
	applied := c.evaluateLocked(at)
	if err := c.saveLocked(); err != nil {
		return nil, err
	}
	return applied, nil
}

// evaluateLocked 是唯一的裁决入口：在给定时刻推进所有到期延迟动作，
// 然后按规则顺序评估持续条件。整个过程按固定点方式重复，直到没有新变化，
// 从而允许动作生成的状态触发其他规则，同时每轮每条规则最多触发一次，
// 保证相互触发的规则不会无限循环。
func (c *Core) evaluateLocked(at time.Time) []Action {
	var applied []Action
	for pass := 0; pass < maxEvaluationPasses; pass++ {
		changed := c.expireBlocksLocked(at)
		changed = c.expireOverridesLocked(at) || changed
		dueChanged := c.processDueLocked(at, &applied)
		fireChanged := c.evaluateRulesLocked(at)
		groupChanged := c.evaluateGroupsLocked(at)
		if !changed && !dueChanged && !fireChanged && !groupChanged {
			break
		}
	}
	c.arbitrateLocked(at)
	return applied
}

// expireBlocksLocked 解除已到期的人工覆盖。TTL 覆盖到期后规则重新开始观察
// 条件：必须先再次看到条件为真并持续满足，才会产生新动作。
func (c *Core) expireBlocksLocked(at time.Time) bool {
	changed := false
	for id, runtime := range c.runtime {
		if runtime.BlockedUntil != nil && !runtime.BlockedUntil.After(at) {
			c.runtime[id] = ruleRuntime{}
			changed = true
		}
	}
	return changed
}

// processDueLocked 执行（或取消）所有到点的延迟动作。
// 手动操作结束后绝不能直接执行早已失效的动作：到点时重新检查
// 规则是否仍被人工覆盖、条件是否仍然成立，任一不满足即取消并记录原因。
func (c *Core) processDueLocked(at time.Time, applied *[]Action) bool {
	changed := false
	winners := c.arbitrationWinnersLocked()
	for _, action := range c.orderedPendingLocked() {
		if action.CompleteAt.After(at) {
			continue
		}
		rule, hasRule := c.rules[action.RuleID]
		if !hasRule {
			c.cancelPendingLocked(action, at, ReasonConditionFalse)
			changed = true
			continue
		}
		if c.isBlockedLocked(c.runtime[action.RuleID], at) {
			c.cancelPendingLocked(action, at, ReasonRuleBlocked)
			changed = true
			continue
		}
		if override := c.overrideCoveringLocked(action.DeviceID, action.Name); override != nil {
			c.cancelPendingWithOverrideLocked(action, at, ReasonRuleBlocked, override.ID)
			changed = true
			continue
		}
		if action.ExpiresAt != nil && at.After(*action.ExpiresAt) {
			// 到点时刻已过太久（例如停机/重启跨越）：动作过时，
			// 记录本应发生的时刻，但不补发。
			delete(c.pending, action.ID)
			c.recordDecisionLocked(Decision{
				At: at, Outcome: OutcomeCanceled, Reason: ReasonStaleAction,
				RuleID: action.RuleID, ActionID: action.ID,
				DeviceID: action.DeviceID, Name: action.Name, Value: &action.Value,
				Detail: fmt.Sprintf("due at %s, expired at %s; not reissued",
					action.CompleteAt.Format(time.RFC3339), action.ExpiresAt.Format(time.RFC3339)),
			})
			c.noteActionCanceledLocked(action, at, ReasonStaleAction, "")
			changed = true
			continue
		}
		if c.snapshotLocked().Offline(action.DeviceID) {
			c.cancelPendingLocked(action, at, ReasonDeviceOffline)
			changed = true
			continue
		}
		if !rule.Condition.Evaluate(c.snapshotLocked()) {
			c.cancelPendingLocked(action, at, ReasonConditionFalse)
			changed = true
			continue
		}
		if winners[arbitrationKey{action.DeviceID, action.Name}] != action.ID {
			c.cancelPendingLocked(action, at, ReasonSuperseded)
			changed = true
			continue
		}
		delete(c.pending, action.ID)
		c.applyPendingLocked(action, at)
		*applied = append(*applied, action)
		changed = true
	}
	return changed
}

type arbitrationKey struct{ device, name string }

// arbitrationWinnersLocked 为每个被控属性选出当前待执行动作的裁决赢家。
func (c *Core) arbitrationWinnersLocked() map[arbitrationKey]string {
	winners := make(map[arbitrationKey]string)
	for _, action := range c.orderedPendingLocked() {
		key := arbitrationKey{action.DeviceID, action.Name}
		winnerID, exists := winners[key]
		if !exists {
			winners[key] = action.ID
			continue
		}
		if winnerID == action.ID {
			continue
		}
		winner := c.pending[winnerID]
		if winner.Value == action.Value {
			continue
		}
		if c.actionWinsLocked(action, winner) {
			winners[key] = action.ID
		}
	}
	return winners
}

// evaluateRulesLocked 按稳定顺序评估一次所有规则的持续条件。
func (c *Core) evaluateRulesLocked(at time.Time) bool {
	changed := false
	for _, rule := range c.orderedRulesLocked() {
		runtime := c.runtime[rule.ID]
		matched := rule.Condition.Evaluate(c.snapshotLocked())
		if !matched {
			// 条件变假会解除“直到重新装弹”式人工覆盖，并重置持续计时。
			if runtime.Satisfied || runtime.Fired || runtime.Blocked {
				c.runtime[rule.ID] = ruleRuntime{}
				changed = true
			}
			continue
		}
		// TTL 覆盖期内条件即使仍为真也不能触发；TTL=0 的覆盖已在上面
		// 借助“条件变假”解除。
		if c.isBlockedLocked(runtime, at) {
			continue
		}
		if c.ruleCoveredByOverrideLocked(rule) != nil {
			continue
		}
		if !runtime.Satisfied {
			runtime.Satisfied = true
			runtime.Since = at
			runtime.Fired = false
			c.runtime[rule.ID] = runtime
			changed = true
			continue
		}
		if runtime.Fired || at.Sub(runtime.Since) < rule.Duration {
			continue
		}
		runtime.Fired = true
		c.runtime[rule.ID] = runtime
		if len(rule.Steps) > 0 {
			// 动作组规则：开启一次运行，由组逻辑逐步推进与确认。
			c.startGroupLocked(rule, at)
			changed = true
			continue
		}
		action := c.enqueueLocked(rule, at)
		if action.CompleteAt.After(at) {
			c.recordDecisionLocked(Decision{
				At: at, Outcome: OutcomeScheduled, Reason: ReasonConditionHeld,
				RuleID: rule.ID, ActionID: action.ID,
				DeviceID: action.DeviceID, Name: action.Name, Value: &action.Value,
			})
			changed = true
			continue
		}
		// 无延迟动作直接视为已到点，交给下一轮 processDue 做重校验与裁决。
		changed = true
	}
	return changed
}

// arbitrateLocked 裁决同一设备同一属性上互相冲突的待执行动作：
// 优先级高者胜，相同优先级规则 ID 较小者稳定获胜，其余动作取消。
// 目标值相同的动作不视为冲突，较早的一个保留，重复项去重。
func (c *Core) arbitrateLocked(at time.Time) {
	winners := c.arbitrationWinnersLocked()
	seen := make(map[string]bool)
	for _, action := range c.orderedPendingLocked() {
		key := arbitrationKey{action.DeviceID, action.Name}
		winnerID := winners[key]
		if winnerID == action.ID {
			if seen[winnerID] {
				c.cancelPendingLocked(action, at, ReasonDuplicate)
			}
			seen[action.ID] = true
			continue
		}
		winner := c.pending[winnerID]
		winnerRule := c.rules[winner.RuleID]
		loserRule := c.rules[action.RuleID]
		// 说明冲突裁决依据：谁胜出、双方规则与优先级，便于核对等待/冲突原因。
		detail := fmt.Sprintf("winner %s (rule %s, priority %d) over rule %s (priority %d)",
			winner.ID, winner.RuleID, winnerRule.Priority, action.RuleID, loserRule.Priority)
		if winner.Value == action.Value {
			c.cancelPendingWithDetailLocked(action, at, ReasonDuplicate, detail)
		} else {
			c.cancelPendingWithDetailLocked(action, at, ReasonSuperseded, detail)
		}
	}
}

func (c *Core) cancelPendingWithDetailLocked(action Action, at time.Time, reason Reason, detail string) {
	delete(c.pending, action.ID)
	c.recordDecisionLocked(Decision{
		At: at, Outcome: OutcomeCanceled, Reason: reason,
		RuleID: action.RuleID, ActionID: action.ID,
		DeviceID: action.DeviceID, Name: action.Name, Value: &action.Value,
		RunID: action.RunID, StepID: action.StepID,
		Detail: detail,
	})
	c.noteActionCanceledLocked(action, at, reason, "")
}

func (c *Core) actionWinsLocked(candidate, incumbent Action) bool {
	candidateRule := c.rules[candidate.RuleID]
	incumbentRule := c.rules[incumbent.RuleID]
	if candidateRule.Priority != incumbentRule.Priority {
		return candidateRule.Priority > incumbentRule.Priority
	}
	if candidateRule.ID != incumbentRule.ID {
		return candidateRule.ID < incumbentRule.ID
	}
	return candidate.ID < incumbent.ID
}

func (c *Core) isBlockedLocked(runtime ruleRuntime, at time.Time) bool {
	if runtime.BlockedUntil != nil {
		return runtime.BlockedUntil.After(at)
	}
	return runtime.Blocked
}

func (c *Core) cancelPendingLocked(action Action, at time.Time, reason Reason) {
	delete(c.pending, action.ID)
	c.recordDecisionLocked(Decision{
		At: at, Outcome: OutcomeCanceled, Reason: reason,
		RuleID: action.RuleID, ActionID: action.ID,
		DeviceID: action.DeviceID, Name: action.Name, Value: &action.Value,
		RunID: action.RunID, StepID: action.StepID,
	})
	c.noteActionCanceledLocked(action, at, reason, "")
}

func (c *Core) cancelPendingWithOverrideLocked(action Action, at time.Time, reason Reason, overrideID string) {
	delete(c.pending, action.ID)
	c.recordDecisionLocked(Decision{
		At: at, Outcome: OutcomeCanceled, Reason: reason,
		RuleID: action.RuleID, ActionID: action.ID,
		DeviceID: action.DeviceID, Name: action.Name, Value: &action.Value,
		OverrideID: overrideID,
		RunID:      action.RunID, StepID: action.StepID,
	})
	c.noteActionCanceledLocked(action, at, reason, overrideID)
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

// Decisions 返回按时间排列的决策日志：每个动作为何执行、延迟或取消都可核对。
func (c *Core) Decisions() []Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Decision(nil), c.decisions...)
}

// History 返回已接收的原始事件（设备上报与人工操作），可用于离线重放。
func (c *Core) History() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.history...)
}

// Replay 用一段旧事件历史在空核上确定性重放，返回推进到 until 时刻得到的核。
// 规则必须与历史录制时一致；事件按自身时间戳处理，不依赖实时时钟。
// until 为零值时只推进到最后一个事件的时刻。
func Replay(store Store, rules []Rule, history []Event, until time.Time) (*Core, []Decision, error) {
	core, err := NewCore(store, nil)
	if err != nil {
		return nil, nil, err
	}
	for _, rule := range rules {
		if err := core.AddRule(rule); err != nil {
			return nil, nil, err
		}
	}
	ordered := make([]Event, 0, len(history))
	for _, event := range history {
		if !until.IsZero() && event.Time.After(until) {
			continue
		}
		ordered = append(ordered, event)
	}
	sortEvents(ordered)
	for _, event := range ordered {
		if event.Tick {
			if _, err := core.Tick(event.Time); err != nil {
				return nil, nil, err
			}
			continue
		}
		// 时钟推进已由历史中的 Tick 条目精确复现；此处按与实时
		// 运行相同的顺序直接处理事件（先应用事件，再评估裁决）。
		if _, err := core.Process(event); err != nil {
			return nil, nil, err
		}
	}
	last := until
	if last.IsZero() && len(ordered) > 0 {
		last = ordered[len(ordered)-1].Time
	}
	if !last.IsZero() {
		if _, err := core.Tick(last); err != nil {
			return nil, nil, err
		}
	}
	return core, core.Decisions(), nil
}

func (c *Core) applyEventLocked(event Event, at time.Time) {
	if event.Override != nil {
		c.applyTakeoverLocked(*event.Override, at)
		return
	}
	if event.RevokeID != "" {
		c.applyRevokeLocked(event.RevokeID, at)
		return
	}
	device := c.devices[event.DeviceID]
	device.ID = event.DeviceID
	if len(event.DeviceType) > 0 {
		device.Types = append([]string(nil), event.DeviceType...)
	}
	if event.Room != "" {
		device.Room = event.Room
	}
	if device.Values == nil {
		device.Values = make(map[string]ValueState)
	}
	device.Values[event.Name] = ValueState{Value: event.Value, LastSource: event.Source}
	c.devices[event.DeviceID] = device

	if event.Source != SourceManual {
		return
	}
	// 人工操作立即取消同设备同属性的待执行自动化动作，
	// 避免延迟动作在人工接管后落地。
	for id, action := range c.pending {
		if action.DeviceID == event.DeviceID && action.Name == event.Name {
			deleted := c.pending[id]
			c.cancelPendingLocked(deleted, at, ReasonManualIntervention)
		}
	}
	// 对所有会控制该设备属性的规则建立人工覆盖。ManualTTL > 0 时覆盖
	// 只在该期限内有效；ManualTTL == 0 时持续到条件重新变假并再次为真。
	for id, rule := range c.rules {
		if rule.Action.DeviceID == event.DeviceID && rule.Action.Name == event.Name {
			runtime := c.runtime[id]
			runtime = ruleRuntime{Blocked: true, Satisfied: false, Fired: false}
			if rule.ManualTTL > 0 {
				until := at.Add(rule.ManualTTL)
				runtime.BlockedUntil = &until
			}
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
	if rule.Action.ValidFor > 0 {
		expiresAt := action.CompleteAt.Add(rule.Action.ValidFor)
		action.ExpiresAt = &expiresAt
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
	c.recordDecisionLocked(Decision{
		At: at, Outcome: OutcomeExecuted, Reason: ReasonConditionHeld,
		RuleID: action.RuleID, ActionID: action.ID,
		DeviceID: action.DeviceID, Name: action.Name, Value: &action.Value,
		RunID: action.RunID, StepID: action.StepID,
	})
	c.groupActionExecutedLocked(action, at)
}

func (c *Core) recordDecisionLocked(decision Decision) {
	c.decisions = append(c.decisions, decision)
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
		Devices:   c.devices,
		Rules:     c.runtime,
		Actions:   c.pending,
		Overrides: c.overrides,
		Seq:       c.seq,
		History:   c.history,
		Applied:   c.applied,
		Decisions: c.decisions,
	})
}
