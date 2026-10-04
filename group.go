package automation

import (
	"fmt"
	"sort"
	"time"
)

// StepState 区分动作组某一步的确切去向：目标已下发、设备已确认效果、
// 效果仍不确定是三种不同状态，不能因为规则触发就把整组记成完成。
type StepState string

const (
	StepPending   StepState = "pending"   // 准备中：等待前序步骤确认或等待共享设备
	StepIssued    StepState = "issued"    // 目标已下发，等待设备回执确认
	StepConfirmed StepState = "confirmed" // 设备已确认效果
	StepUncertain StepState = "uncertain" // 期限内未确认（含设备离线），效果不确定
	StepCanceled  StepState = "canceled"  // 被人工接管、冲突裁决或组中止取消
	StepSkipped   StepState = "skipped"   // 当前观察已满足目标，无需执行
)

func isTerminalStep(state StepState) bool {
	switch state {
	case StepConfirmed, StepUncertain, StepCanceled, StepSkipped:
		return true
	}
	return false
}

// StepStatus 是动作组运行中某一步的实时状态。
type StepStatus struct {
	Action         SetStateAction `json:"action"`
	ConfirmTimeout time.Duration  `json:"confirm_timeout,omitempty"`
	State          StepState      `json:"state"`
	ActionID       string         `json:"action_id,omitempty"`
	IssuedAt       *time.Time     `json:"issued_at,omitempty"`
	ConfirmBy      *time.Time     `json:"confirm_by,omitempty"`
	ResolvedAt     *time.Time     `json:"resolved_at,omitempty"`
	// WaitingFor 非空表示该步骤正等待另一个动作组释放同一设备属性。
	WaitingFor string `json:"waiting_for,omitempty"`
	// OverrideID 记录取消该步骤的人工接管层（如适用）。
	OverrideID string `json:"override_id,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// GroupRun 是规则一次触发产生的动作组运行实例：每个效果都能追溯到
// 这次具体的规则判断（GroupID 含规则 ID 与单调序号）。
type GroupRun struct {
	ID        string       `json:"id"`
	RuleID    string       `json:"rule_id"`
	StartedAt time.Time    `json:"started_at"`
	Steps     []StepStatus `json:"steps"`
}

// Groups 返回全部动作组运行（按 ID 排序，结果确定），供审计与展示。
func (c *Core) Groups() []GroupRun {
	c.mu.Lock()
	defer c.mu.Unlock()
	ordered := c.orderedGroupsLocked()
	runs := make([]GroupRun, 0, len(ordered))
	for _, group := range ordered {
		copyRun := *group
		copyRun.Steps = append([]StepStatus(nil), group.Steps...)
		runs = append(runs, copyRun)
	}
	return runs
}

func (c *Core) orderedGroupsLocked() []*GroupRun {
	ids := make([]string, 0, len(c.groups))
	for id := range c.groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ordered := make([]*GroupRun, 0, len(ids))
	for _, id := range ids {
		ordered = append(ordered, c.groups[id])
	}
	return ordered
}

// ruleTargets 返回规则会控制的全部设备属性（单动作或动作组各步）。
func ruleTargets(rule Rule) []SetStateAction {
	if len(rule.Steps) == 0 {
		return []SetStateAction{rule.Action}
	}
	targets := make([]SetStateAction, 0, len(rule.Steps))
	for _, step := range rule.Steps {
		targets = append(targets, step.Action)
	}
	return targets
}

// ruleControls 判断规则是否会控制指定设备属性。
func ruleControls(rule Rule, deviceID, name string) bool {
	for _, target := range ruleTargets(rule) {
		if target.DeviceID == deviceID && target.Name == name {
			return true
		}
	}
	return false
}

// ruleCoveredLocked 返回罩住规则任一控制目标的第一层接管。
func (c *Core) ruleCoveredLocked(rule Rule) *Override {
	for _, target := range ruleTargets(rule) {
		if override := c.overrideCoveringLocked(target.DeviceID, target.Name); override != nil {
			return override
		}
	}
	return nil
}

// startGroupLocked 在规则触发时创建一次动作组运行，并尝试启动第一步。
func (c *Core) startGroupLocked(rule Rule, at time.Time) {
	c.seq++
	group := &GroupRun{ID: fmt.Sprintf("%s-g%d", rule.ID, c.seq), RuleID: rule.ID, StartedAt: at}
	for _, step := range rule.Steps {
		group.Steps = append(group.Steps, StepStatus{
			Action:         step.Action,
			ConfirmTimeout: step.ConfirmTimeout,
			State:          StepPending,
		})
	}
	c.groups[group.ID] = group
	c.recordDecisionLocked(Decision{
		At: at, Outcome: OutcomeScheduled, Reason: ReasonConditionHeld,
		RuleID: rule.ID, GroupID: group.ID,
		Detail: fmt.Sprintf("action group started with %d steps", len(group.Steps)),
	})
	c.tryStartStepLocked(group, 0, at)
}

// tryStartStepLocked 尝试启动动作组的第 idx 步：先按当前观察判断是否
// 已经满足（满足则跳过），再检查共享设备冲突，最后排定延迟动作。
func (c *Core) tryStartStepLocked(group *GroupRun, idx int, at time.Time) {
	if idx >= len(group.Steps) {
		return
	}
	step := &group.Steps[idx]
	// 共享设备：其他动作组在同一属性上还有未确认的效果时，必须说明
	// 等待或冲突的依据，独立设备上的步骤不受影响、继续推进。
	for _, other := range c.orderedGroupsLocked() {
		if other.ID == group.ID {
			continue
		}
		for otherIdx := range other.Steps {
			otherStep := &other.Steps[otherIdx]
			if otherStep.State != StepIssued ||
				otherStep.Action.DeviceID != step.Action.DeviceID ||
				otherStep.Action.Name != step.Action.Name {
				continue
			}
			if otherStep.Action.Value == step.Action.Value {
				if step.WaitingFor != other.ID {
					step.WaitingFor = other.ID
					c.recordStepDecisionLocked(group, idx, at, OutcomeScheduled, ReasonWaitSharedDevice, "",
						fmt.Sprintf("waiting for group %s to confirm %s.%s", other.ID, step.Action.DeviceID, step.Action.Name))
				}
				return
			}
			// 目标值不同即冲突：按规则优先级裁决，败方步骤取消并注明依据。
			otherRule := c.rules[other.RuleID]
			rule := c.rules[group.RuleID]
			if otherRule.Priority >= rule.Priority {
				detail := fmt.Sprintf("conflicts with issued step of group %s (priority %d >= %d)",
					other.ID, otherRule.Priority, rule.Priority)
				c.cancelGroupStepLocked(group, idx, StepCanceled, at, ReasonSuperseded, "", detail)
				return
			}
			detail := fmt.Sprintf("preempted by group %s step %d (priority %d > %d)",
				group.ID, idx, rule.Priority, otherRule.Priority)
			c.cancelGroupStepLocked(other, otherIdx, StepCanceled, at, ReasonSuperseded, "", detail)
		}
	}
	// 按当前观察重新判断：设备已处于目标状态时该步无需执行。
	if value, ok := c.snapshotLocked().Value(step.Action.DeviceID, step.Action.Name); ok && value == step.Action.Value {
		step.State = StepSkipped
		step.ResolvedAt = &at
		step.Detail = "observed already at target"
		c.recordStepDecisionLocked(group, idx, at, OutcomeExecuted, ReasonAlreadySatisfied, "", step.Detail)
		c.tryStartStepLocked(group, idx+1, at)
		return
	}
	step.WaitingFor = ""
	c.seq++
	action := Action{
		ID:         fmt.Sprintf("%s-s%d", group.ID, idx),
		RuleID:     group.RuleID,
		DeviceID:   step.Action.DeviceID,
		Name:       step.Action.Name,
		Value:      step.Action.Value,
		CompleteAt: at.Add(step.Action.After),
		GroupID:    group.ID,
		StepIndex:  idx,
	}
	if step.Action.ValidFor > 0 {
		expiresAt := action.CompleteAt.Add(step.Action.ValidFor)
		action.ExpiresAt = &expiresAt
	}
	step.ActionID = action.ID
	c.pending[action.ID] = action
	if action.CompleteAt.After(at) {
		c.recordDecisionLocked(Decision{
			At: at, Outcome: OutcomeScheduled, Reason: ReasonConditionHeld,
			RuleID: group.RuleID, ActionID: action.ID, GroupID: group.ID, StepIndex: intPtr(idx),
			DeviceID: action.DeviceID, Name: action.Name, Value: &action.Value,
		})
	}
}

// markStepIssuedLocked 在延迟动作到点落地后把步骤标记为“目标已下发”，
// 并按 ConfirmTimeout 决定是立即确认还是等待设备回执。
func (c *Core) markStepIssuedLocked(action Action, at time.Time) {
	group, ok := c.groups[action.GroupID]
	if !ok {
		return
	}
	step := &group.Steps[action.StepIndex]
	if isTerminalStep(step.State) {
		return
	}
	step.State = StepIssued
	step.IssuedAt = &at
	if step.ConfirmTimeout <= 0 {
		c.confirmStepLocked(group, action.StepIndex, at)
		return
	}
	confirmBy := at.Add(step.ConfirmTimeout)
	step.ConfirmBy = &confirmBy
}

// confirmStepLocked 把步骤标记为“设备已确认效果”，并推进组的下一步。
func (c *Core) confirmStepLocked(group *GroupRun, idx int, at time.Time) {
	step := &group.Steps[idx]
	step.State = StepConfirmed
	step.ResolvedAt = &at
	c.recordStepDecisionLocked(group, idx, at, OutcomeExecuted, ReasonStepConfirmed, "", "")
	c.tryStartStepLocked(group, idx+1, at)
}

// confirmStepsLocked 用设备上报回执确认等待中的步骤。
func (c *Core) confirmStepsLocked(event Event, at time.Time) {
	for _, group := range c.orderedGroupsLocked() {
		for idx := range group.Steps {
			step := &group.Steps[idx]
			if step.State != StepIssued ||
				step.Action.DeviceID != event.DeviceID ||
				step.Action.Name != event.Name ||
				event.Value != step.Action.Value {
				continue
			}
			c.confirmStepLocked(group, idx, at)
		}
	}
}

// checkConfirmationsLocked 把超过确认期限仍未收到回执的步骤记为不确定，
// 并中止该组的后续步骤（后续动作不得在前置效果不确定时继续下发）。
func (c *Core) checkConfirmationsLocked(at time.Time) bool {
	changed := false
	for _, group := range c.orderedGroupsLocked() {
		for idx := range group.Steps {
			step := &group.Steps[idx]
			if step.State != StepIssued || step.ConfirmBy == nil || step.ConfirmBy.After(at) {
				continue
			}
			reason := ReasonStepUnconfirmed
			if c.snapshotLocked().Offline(step.Action.DeviceID) {
				reason = ReasonDeviceOffline
			}
			step.State = StepUncertain
			step.ResolvedAt = &at
			step.Detail = fmt.Sprintf("confirmation not received by %s", step.ConfirmBy.Format(time.RFC3339))
			c.recordStepDecisionLocked(group, idx, at, OutcomeUncertain, reason, "", step.Detail)
			c.haltGroupLocked(group, idx+1, at, reason, "")
			changed = true
		}
	}
	return changed
}

// retryWaitingStepsLocked 在共享设备的占用解除后重试等待中的步骤。
func (c *Core) retryWaitingStepsLocked(at time.Time) bool {
	changed := false
	for _, group := range c.orderedGroupsLocked() {
		for idx := range group.Steps {
			step := &group.Steps[idx]
			if step.State != StepPending || step.WaitingFor == "" {
				continue
			}
			blocker, ok := c.groups[step.WaitingFor]
			stillBlocked := false
			if ok {
				for _, blockerStep := range blocker.Steps {
					if blockerStep.State == StepIssued &&
						blockerStep.Action.DeviceID == step.Action.DeviceID &&
						blockerStep.Action.Name == step.Action.Name {
						stillBlocked = true
					}
				}
			}
			if stillBlocked {
				continue
			}
			step.WaitingFor = ""
			c.tryStartStepLocked(group, idx, at)
			changed = true
		}
	}
	return changed
}

// resolveGroupStepLocked 在步骤对应的延迟动作被取消后同步步骤状态，
// 并中止该组后续步骤。决策已由取消路径记录，这里只更新运行状态。
func (c *Core) resolveGroupStepLocked(groupID string, idx int, state StepState, at time.Time, reason Reason, overrideID string) {
	group, ok := c.groups[groupID]
	if !ok || idx >= len(group.Steps) {
		return
	}
	step := &group.Steps[idx]
	if isTerminalStep(step.State) {
		return
	}
	step.State = state
	step.ResolvedAt = &at
	step.OverrideID = overrideID
	c.haltGroupLocked(group, idx+1, at, reason, overrideID)
}

// cancelGroupStepLocked 取消一个没有对应挂起动作的活动步骤
// （已下发待确认或等待共享设备），记录决策并中止后续步骤。
func (c *Core) cancelGroupStepLocked(group *GroupRun, idx int, state StepState, at time.Time, reason Reason, overrideID, detail string) {
	step := &group.Steps[idx]
	if isTerminalStep(step.State) {
		return
	}
	if step.ActionID != "" {
		delete(c.pending, step.ActionID)
	}
	step.State = state
	step.ResolvedAt = &at
	step.OverrideID = overrideID
	step.Detail = detail
	c.recordStepDecisionLocked(group, idx, at, outcomeForStep(state), reason, overrideID, detail)
	c.haltGroupLocked(group, idx+1, at, reason, overrideID)
}

// haltGroupLocked 取消组内从 from 起所有尚未终结的步骤（人工接管或
// 前置步骤不确定时，后续动作一律停止，不再下发）。
func (c *Core) haltGroupLocked(group *GroupRun, from int, at time.Time, reason Reason, overrideID string) {
	for idx := from; idx < len(group.Steps); idx++ {
		step := &group.Steps[idx]
		if isTerminalStep(step.State) {
			continue
		}
		if step.ActionID != "" {
			delete(c.pending, step.ActionID)
		}
		step.State = StepCanceled
		step.ResolvedAt = &at
		step.OverrideID = overrideID
		c.recordStepDecisionLocked(group, idx, at, OutcomeCanceled, reason, overrideID, "group halted")
	}
}

func (c *Core) recordStepDecisionLocked(group *GroupRun, idx int, at time.Time, outcome Outcome, reason Reason, overrideID, detail string) {
	step := group.Steps[idx]
	value := step.Action.Value
	c.recordDecisionLocked(Decision{
		At: at, Outcome: outcome, Reason: reason,
		RuleID: group.RuleID, GroupID: group.ID, StepIndex: intPtr(idx),
		DeviceID: step.Action.DeviceID, Name: step.Action.Name, Value: &value,
		OverrideID: overrideID, Detail: detail,
	})
}

func outcomeForStep(state StepState) Outcome {
	if state == StepUncertain {
		return OutcomeUncertain
	}
	return OutcomeCanceled
}

func stepStateForReason(reason Reason) StepState {
	if reason == ReasonDeviceOffline {
		return StepUncertain
	}
	return StepCanceled
}

func intPtr(v int) *int { return &v }
