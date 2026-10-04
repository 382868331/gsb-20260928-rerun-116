package automation

import (
	"fmt"
	"time"
)

// 步骤状态：目标已定义（pending/waiting_confirm）、设备已确认效果
// （confirmed/skipped）、仍不确定（uncertain）以及已取消（canceled）。
const (
	StepPending        = "pending"
	StepWaitingConfirm = "waiting_confirm"
	StepConfirmed      = "confirmed"
	StepUncertain      = "uncertain"
	StepSkipped        = "skipped"
	StepCanceled       = "canceled"
)

type stepState struct {
	StepID string `json:"step_id"`
	Status string `json:"status"`
}

// groupRuntime 是一次动作组运行的持久化状态：重启后据此继续核对
// 未完成的确认等待，而不是从头补发旧动作。
type groupRuntime struct {
	RunID           string      `json:"run_id"`
	StepIndex       int         `json:"step_index"`
	Steps           []stepState `json:"steps"`
	PendingActionID string      `json:"pending_action_id,omitempty"`
	ConfirmDeadline *time.Time  `json:"confirm_deadline,omitempty"`
	Done            bool        `json:"done"`
}

// StepStatus 是步骤状态的只读视图。
type StepStatus struct {
	StepID string `json:"step_id"`
	Status string `json:"status"`
}

// GroupRun 是一次动作组运行的只读视图，用于核对每一步的当前状态。
type GroupRun struct {
	RuleID    string       `json:"rule_id"`
	RunID     string       `json:"run_id"`
	StepIndex int          `json:"step_index"`
	Steps     []StepStatus `json:"steps"`
	Done      bool         `json:"done"`
}

// GroupRuns 返回当前各规则最近一次动作组运行的状态（按规则 ID 排序）。
func (c *Core) GroupRuns() []GroupRun {
	c.mu.Lock()
	defer c.mu.Unlock()
	var runs []GroupRun
	for _, rule := range c.orderedRulesLocked() {
		gr := c.runtime[rule.ID].Group
		if gr == nil {
			continue
		}
		run := GroupRun{RuleID: rule.ID, RunID: gr.RunID, StepIndex: gr.StepIndex, Done: gr.Done}
		for _, step := range gr.Steps {
			run.Steps = append(run.Steps, StepStatus{StepID: step.StepID, Status: step.Status})
		}
		runs = append(runs, run)
	}
	return runs
}

// startGroupLocked 在规则触发时开启一次动作组运行，并立即从当前观察
// 推进尽可能多的步骤（已满足的目标直接跳过，不补发动作）。
func (c *Core) startGroupLocked(rule Rule, at time.Time) {
	c.seq++
	runID := fmt.Sprintf("%s-run-%d", rule.ID, c.seq)
	gr := &groupRuntime{RunID: runID, Steps: make([]stepState, len(rule.Steps))}
	for i, step := range rule.Steps {
		gr.Steps[i] = stepState{StepID: step.ID, Status: StepPending}
	}
	runtime := c.runtime[rule.ID]
	runtime.Group = gr
	c.runtime[rule.ID] = runtime
	c.recordDecisionLocked(Decision{
		At: at, Outcome: OutcomeGroup, Reason: ReasonGroupStarted,
		RuleID: rule.ID, RunID: runID,
	})
	c.advanceGroupLocked(rule, at)
}

// advanceGroupLocked 按序推进组内步骤：已满足的目标跳过、确认步骤进入
// 等待、动作步骤排定延迟动作。整组全部确认后才记录 group_completed。
func (c *Core) advanceGroupLocked(rule Rule, at time.Time) bool {
	runtime := c.runtime[rule.ID]
	gr := runtime.Group
	if gr == nil || gr.Done {
		return false
	}
	snapshot := c.snapshotLocked()
	for gr.StepIndex < len(rule.Steps) {
		step := rule.Steps[gr.StepIndex]
		if step.Verify != nil {
			condition := DeviceProperty{
				DeviceID: step.Verify.DeviceID, Name: step.Verify.Name,
				Operator: step.Verify.Operator, Value: step.Verify.Value,
			}
			if condition.Evaluate(snapshot) {
				gr.Steps[gr.StepIndex].Status = StepConfirmed
				c.recordDecisionLocked(Decision{
					At: at, Outcome: OutcomeGroup, Reason: ReasonStepConfirmed,
					RuleID: rule.ID, RunID: gr.RunID, StepID: step.ID,
					DeviceID: step.Verify.DeviceID, Name: step.Verify.Name, Value: &step.Verify.Value,
					Detail: "verified from current observation",
				})
				gr.StepIndex++
				continue
			}
			deadline := at.Add(step.Verify.Within)
			gr.ConfirmDeadline = &deadline
			gr.Steps[gr.StepIndex].Status = StepWaitingConfirm
			runtime.Group = gr
			c.runtime[rule.ID] = runtime
			return true
		}
		if value, ok := snapshot.Value(step.Action.DeviceID, step.Action.Name); ok && value == step.Action.Value {
			// 当前观察下目标已满足：不补发动作，直接记为已确认。
			gr.Steps[gr.StepIndex].Status = StepSkipped
			c.recordDecisionLocked(Decision{
				At: at, Outcome: OutcomeGroup, Reason: ReasonStepSkipped,
				RuleID: rule.ID, RunID: gr.RunID, StepID: step.ID,
				DeviceID: step.Action.DeviceID, Name: step.Action.Name, Value: &step.Action.Value,
				Detail: "goal already satisfied; action not reissued",
			})
			gr.StepIndex++
			continue
		}
		action := c.enqueueStepLocked(rule, step, gr, at)
		gr.PendingActionID = action.ID
		gr.Steps[gr.StepIndex].Status = StepPending
		runtime.Group = gr
		c.runtime[rule.ID] = runtime
		return true
	}
	gr.Done = true
	runtime.Group = gr
	c.runtime[rule.ID] = runtime
	c.recordDecisionLocked(Decision{
		At: at, Outcome: OutcomeGroup, Reason: ReasonGroupCompleted,
		RuleID: rule.ID, RunID: gr.RunID,
		Detail: "all steps confirmed",
	})
	return true
}

// evaluateGroupsLocked 每次裁决时推进所有进行中的动作组：
// 检查取消条件（条件失效 / 人工覆盖 / 接管层）、确认等待的结果与超时。
func (c *Core) evaluateGroupsLocked(at time.Time) bool {
	changed := false
	for _, rule := range c.orderedRulesLocked() {
		if len(rule.Steps) == 0 {
			continue
		}
		runtime := c.runtime[rule.ID]
		gr := runtime.Group
		if gr == nil || gr.Done {
			continue
		}
		snapshot := c.snapshotLocked()
		if !rule.Condition.Evaluate(snapshot) {
			c.finishGroupLocked(rule, at, ReasonConditionFalse, "", "condition no longer holds")
			changed = true
			continue
		}
		if c.isBlockedLocked(runtime, at) {
			c.finishGroupLocked(rule, at, ReasonRuleBlocked, "", "rule blocked by manual control")
			changed = true
			continue
		}
		if override := c.groupCoveringOverrideLocked(rule, gr); override != nil {
			c.finishGroupLocked(rule, at, ReasonManualIntervention, override.ID,
				"remaining steps covered by override "+override.ID)
			changed = true
			continue
		}
		if gr.ConfirmDeadline == nil {
			continue
		}
		step := rule.Steps[gr.StepIndex]
		if c.stepConfirmedLocked(step, snapshot) {
			gr.ConfirmDeadline = nil
			gr.Steps[gr.StepIndex].Status = StepConfirmed
			gr.StepIndex++
			c.recordDecisionLocked(Decision{
				At: at, Outcome: OutcomeGroup, Reason: ReasonStepConfirmed,
				RuleID: rule.ID, RunID: gr.RunID, StepID: step.ID,
			})
			runtime.Group = gr
			c.runtime[rule.ID] = runtime
			c.advanceGroupLocked(rule, at)
			changed = true
			continue
		}
		if !gr.ConfirmDeadline.After(at) {
			// 确认窗口已过（可能因停机/重启跨越）：步骤记为不确定，
			// 后续步骤不再执行，绝不把整组当作完成。
			deadline := *gr.ConfirmDeadline
			gr.ConfirmDeadline = nil
			c.finishGroupLocked(rule, at, ReasonStepUncertain, "",
				fmt.Sprintf("step %s unconfirmed by %s; uncertain at %s",
					step.ID, deadline.Format(time.RFC3339), at.Format(time.RFC3339)))
			changed = true
		}
	}
	return changed
}

// stepConfirmedLocked 判断步骤的确认条件是否已被当前观察满足。
// 动作步骤要求设备以自身上报（SourceDevice）确认目标值，
// 自动化自己写入的值不算设备确认。
func (c *Core) stepConfirmedLocked(step Step, snapshot Snapshot) bool {
	if step.Verify != nil {
		condition := DeviceProperty{
			DeviceID: step.Verify.DeviceID, Name: step.Verify.Name,
			Operator: step.Verify.Operator, Value: step.Verify.Value,
		}
		return condition.Evaluate(snapshot)
	}
	device, ok := snapshot.Devices[step.Action.DeviceID]
	if !ok || snapshot.Offline(step.Action.DeviceID) {
		return false
	}
	state, ok := device.Values[step.Action.Name]
	return ok && state.Value == step.Action.Value && state.LastSource == SourceDevice
}

// groupCoveringOverrideLocked 返回罩住任一剩余步骤目标设备的第一层接管。
func (c *Core) groupCoveringOverrideLocked(rule Rule, gr *groupRuntime) *Override {
	for i := gr.StepIndex; i < len(rule.Steps); i++ {
		deviceID, name := stepTarget(rule.Steps[i])
		if deviceID == "" {
			continue
		}
		if override := c.overrideCoveringLocked(deviceID, name); override != nil {
			return override
		}
	}
	return nil
}

// stepTarget 返回步骤作用的设备与属性。
func stepTarget(step Step) (string, string) {
	if step.Action != nil {
		return step.Action.DeviceID, step.Action.Name
	}
	if step.Verify != nil {
		return step.Verify.DeviceID, step.Verify.Name
	}
	return "", ""
}

// ruleTargets 返回规则作用的全部设备属性：动作组规则覆盖每一步的目标，
// 单动作规则即其 Action 目标。
func ruleTargets(rule Rule) [][2]string {
	if len(rule.Steps) == 0 {
		return [][2]string{{rule.Action.DeviceID, rule.Action.Name}}
	}
	var targets [][2]string
	for _, step := range rule.Steps {
		deviceID, name := stepTarget(step)
		if deviceID != "" {
			targets = append(targets, [2]string{deviceID, name})
		}
	}
	return targets
}

// ruleCoveredByOverrideLocked 判断规则是否被任一层接管罩住：
// 动作组规则只要有一个步骤目标被罩住即视为被接管。
func (c *Core) ruleCoveredByOverrideLocked(rule Rule) *Override {
	for _, target := range ruleTargets(rule) {
		if override := c.overrideCoveringLocked(target[0], target[1]); override != nil {
			return override
		}
	}
	return nil
}

// finishGroupLocked 终止一次动作组运行：当前及后续步骤全部取消，
// 记录取消原因与涉及的接管层，并让规则重新从头观察条件，
// 以便恢复后由当前观察重新判断哪些步骤仍需要执行。
func (c *Core) finishGroupLocked(rule Rule, at time.Time, reason Reason, overrideID, detail string) {
	runtime := c.runtime[rule.ID]
	gr := runtime.Group
	if gr == nil || gr.Done {
		return
	}
	for i := gr.StepIndex; i < len(gr.Steps); i++ {
		if reason == ReasonStepUncertain && i == gr.StepIndex {
			gr.Steps[i].Status = StepUncertain
			continue
		}
		gr.Steps[i].Status = StepCanceled
	}
	gr.Done = true
	gr.ConfirmDeadline = nil
	runtime.Group = gr
	// 重新装弹：恢复后必须重新观察条件并再次持续满足才会开启新运行。
	runtime.Satisfied = false
	runtime.Fired = false
	c.runtime[rule.ID] = runtime
	stepID := ""
	if gr.StepIndex < len(gr.Steps) {
		stepID = gr.Steps[gr.StepIndex].StepID
	}
	c.recordDecisionLocked(Decision{
		At: at, Outcome: OutcomeGroup, Reason: ReasonGroupCanceled,
		RuleID: rule.ID, RunID: gr.RunID, StepID: stepID, OverrideID: overrideID,
		Detail: string(reason) + ": " + detail,
	})
}

// groupActionExecutedLocked 在组内动作落地后推进运行：
// 需要设备确认时进入等待，否则直接确认并推进下一步。
func (c *Core) groupActionExecutedLocked(action Action, at time.Time) {
	if action.RunID == "" {
		return
	}
	rule, ok := c.rules[action.RuleID]
	if !ok {
		return
	}
	runtime := c.runtime[action.RuleID]
	gr := runtime.Group
	if gr == nil || gr.Done || gr.RunID != action.RunID || gr.PendingActionID != action.ID {
		return
	}
	gr.PendingActionID = ""
	step := rule.Steps[gr.StepIndex]
	if step.ConfirmWithin > 0 {
		deadline := at.Add(step.ConfirmWithin)
		gr.ConfirmDeadline = &deadline
		gr.Steps[gr.StepIndex].Status = StepWaitingConfirm
		runtime.Group = gr
		c.runtime[action.RuleID] = runtime
		return
	}
	gr.Steps[gr.StepIndex].Status = StepConfirmed
	gr.StepIndex++
	c.recordDecisionLocked(Decision{
		At: at, Outcome: OutcomeGroup, Reason: ReasonStepConfirmed,
		RuleID: rule.ID, RunID: gr.RunID, StepID: step.ID,
		DeviceID: action.DeviceID, Name: action.Name, Value: &action.Value,
	})
	runtime.Group = gr
	c.runtime[action.RuleID] = runtime
	c.advanceGroupLocked(rule, at)
}

// noteActionCanceledLocked 在组内挂起动作被取消（人工、接管、离线、
// 过时、被高优先级取代）时终止整个运行：后续步骤不再执行。
func (c *Core) noteActionCanceledLocked(action Action, at time.Time, reason Reason, overrideID string) {
	if action.RunID == "" {
		return
	}
	rule, ok := c.rules[action.RuleID]
	if !ok {
		return
	}
	runtime := c.runtime[action.RuleID]
	gr := runtime.Group
	if gr == nil || gr.Done || gr.RunID != action.RunID || gr.PendingActionID != action.ID {
		return
	}
	gr.PendingActionID = ""
	runtime.Group = gr
	c.runtime[action.RuleID] = runtime
	c.finishGroupLocked(rule, at, reason, overrideID, "pending action "+action.ID+" canceled: "+string(reason))
}

// enqueueStepLocked 为动作步骤排定延迟动作，动作携带运行与步骤标识以便追溯。
func (c *Core) enqueueStepLocked(rule Rule, step Step, gr *groupRuntime, at time.Time) Action {
	c.seq++
	action := Action{
		ID:         fmt.Sprintf("%s-%d", rule.ID, c.seq),
		RuleID:     rule.ID,
		DeviceID:   step.Action.DeviceID,
		Name:       step.Action.Name,
		Value:      step.Action.Value,
		CompleteAt: at.Add(step.Action.After),
		RunID:      gr.RunID,
		StepID:     step.ID,
	}
	if step.Action.ValidFor > 0 {
		expiresAt := action.CompleteAt.Add(step.Action.ValidFor)
		action.ExpiresAt = &expiresAt
	}
	c.pending[action.ID] = action
	c.recordDecisionLocked(Decision{
		At: at, Outcome: OutcomeScheduled, Reason: ReasonConditionHeld,
		RuleID: rule.ID, ActionID: action.ID,
		DeviceID: action.DeviceID, Name: action.Name, Value: &action.Value,
		RunID: gr.RunID, StepID: step.ID,
	})
	return action
}
