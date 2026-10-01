package automation

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// OverrideScope 限定一层人工接管的作用范围：可以按房间整层接管，
// 也可以只针对某个设备、甚至某个设备的单个属性（临时例外）。
// Room 与 DeviceID 至少填一个；Name 为空表示覆盖该范围内全部属性。
type OverrideScope struct {
	Room     string `json:"room,omitempty"`
	DeviceID string `json:"device_id,omitempty"`
	Name     string `json:"name,omitempty"`
}

// Override 是一层独立的人工接管。多层接管可以同时存在（例如房间接管
// 叠加某设备的临时例外），各层的作用范围与到期时间互不影响；某一层
// 到期或被撤销后，其余仍然有效的层继续生效。
type Override struct {
	ID        string        `json:"id"`
	Scope     OverrideScope `json:"scope"`
	Reason    string        `json:"reason,omitempty"`
	StartedAt time.Time     `json:"started_at"`
	// ExpiresAt 为 nil 表示直到显式撤销；到期后该层自动解除。
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// covers 判断该层是否罩住目标设备属性。room 是目标设备所属房间。
func (o Override) covers(deviceID, room, name string) bool {
	if o.Scope.Room == "" && o.Scope.DeviceID == "" {
		return false
	}
	if o.Scope.Room != "" && o.Scope.Room != room {
		return false
	}
	if o.Scope.DeviceID != "" && o.Scope.DeviceID != deviceID {
		return false
	}
	if o.Scope.Name != "" && o.Scope.Name != name {
		return false
	}
	return true
}

// Takeover 建立一层人工接管：立即取消作用范围内所有挂起的自动化动作，
// 并在层有效期内阻止相关规则触发。接管以事件形式进入历史，可持久化、可重放。
func (c *Core) Takeover(override Override, at time.Time) error {
	if override.ID == "" || (override.Scope.Room == "" && override.Scope.DeviceID == "") {
		return errors.New("override requires id and a room or device scope")
	}
	c.mu.Lock()
	if _, exists := c.overrides[override.ID]; exists {
		c.mu.Unlock()
		return fmt.Errorf("override %q already exists", override.ID)
	}
	c.mu.Unlock()
	event := Event{
		DeviceID: override.Scope.DeviceID,
		Room:     override.Scope.Room,
		Name:     override.Scope.Name,
		Source:   SourceManual,
		Time:     at,
		Override: &override,
	}
	_, err := c.Process(event)
	return err
}

// RevokeOverride 撤销指定接管层。其余仍有效的层继续生效；
// 不再被任何层覆盖的规则从头开始观察条件，而不是回到最早的自动规则。
func (c *Core) RevokeOverride(id string, at time.Time) error {
	c.mu.Lock()
	_, exists := c.overrides[id]
	c.mu.Unlock()
	if !exists {
		return fmt.Errorf("override %q not found", id)
	}
	event := Event{Source: SourceManual, Time: at, RevokeID: id}
	_, err := c.Process(event)
	return err
}

// ActiveOverrides 返回当前仍有效的全部接管层（按 ID 排序，结果确定）。
func (c *Core) ActiveOverrides() []Override {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.orderedOverridesLocked()
}

func (c *Core) orderedOverridesLocked() []Override {
	overrides := make([]Override, 0, len(c.overrides))
	for _, override := range c.overrides {
		overrides = append(overrides, override)
	}
	sort.Slice(overrides, func(i, j int) bool { return overrides[i].ID < overrides[j].ID })
	return overrides
}

// overrideCoveringLocked 返回罩住目标设备属性的第一层接管（按 ID 顺序，
// 保证决策日志可复现）；没有任何层覆盖时返回 nil。
func (c *Core) overrideCoveringLocked(deviceID, name string) *Override {
	room := c.devices[deviceID].Room
	for _, override := range c.orderedOverridesLocked() {
		if override.covers(deviceID, room, name) {
			found := override
			return &found
		}
	}
	return nil
}

// applyTakeoverLocked 落地一层接管：登记该层、取消范围内挂起动作并记录决策。
func (c *Core) applyTakeoverLocked(override Override, at time.Time) {
	override.StartedAt = at
	c.overrides[override.ID] = override
	c.recordDecisionLocked(Decision{
		At: at, Outcome: OutcomeOverride, Reason: ReasonOverrideStarted,
		OverrideID: override.ID, DeviceID: override.Scope.DeviceID,
		Name: override.Scope.Name, Detail: override.Reason,
	})
	for _, action := range c.orderedPendingLocked() {
		room := c.devices[action.DeviceID].Room
		if !override.covers(action.DeviceID, room, action.Name) {
			continue
		}
		delete(c.pending, action.ID)
		c.recordDecisionLocked(Decision{
			At: at, Outcome: OutcomeCanceled, Reason: ReasonManualIntervention,
			RuleID: action.RuleID, ActionID: action.ID,
			DeviceID: action.DeviceID, Name: action.Name, Value: &action.Value,
			OverrideID: override.ID,
		})
	}
}

// applyRevokeLocked 撤销一层接管，并让因此不再被覆盖的规则重新观察条件。
func (c *Core) applyRevokeLocked(id string, at time.Time) {
	override, exists := c.overrides[id]
	if !exists {
		return
	}
	delete(c.overrides, id)
	c.recordDecisionLocked(Decision{
		At: at, Outcome: OutcomeOverride, Reason: ReasonOverrideRevoked,
		OverrideID: id, DeviceID: override.Scope.DeviceID, Name: override.Scope.Name,
	})
	c.rearmUncoveredRulesLocked(override)
}

// expireOverridesLocked 解除到期的接管层。决策的 At 记录层本应到期的时刻
// （ExpiresAt），因此重启跨越到期时刻时，日志区分“应当发生的时刻”与
// “实际发现到期的时刻”，且不会补发任何已过时动作。
func (c *Core) expireOverridesLocked(at time.Time) bool {
	changed := false
	for _, override := range c.orderedOverridesLocked() {
		if override.ExpiresAt == nil || override.ExpiresAt.After(at) {
			continue
		}
		delete(c.overrides, override.ID)
		c.recordDecisionLocked(Decision{
			At: *override.ExpiresAt, Outcome: OutcomeOverride, Reason: ReasonOverrideExpired,
			OverrideID: override.ID, DeviceID: override.Scope.DeviceID, Name: override.Scope.Name,
			Detail: fmt.Sprintf("expired during evaluation at %s", at.Format(time.RFC3339)),
		})
		c.rearmUncoveredRulesLocked(override)
		changed = true
	}
	return changed
}

// rearmUncoveredRulesLocked 在一层接管消失后，把不再被任何层覆盖、
// 也未处于旧式人工覆盖中的规则重置为重新观察条件的状态。
func (c *Core) rearmUncoveredRulesLocked(removed Override) {
	for id, rule := range c.rules {
		room := c.devices[rule.Action.DeviceID].Room
		if !removed.covers(rule.Action.DeviceID, room, rule.Action.Name) {
			continue
		}
		if c.overrideCoveringLocked(rule.Action.DeviceID, rule.Action.Name) != nil {
			continue // 仍有其他层罩着，维持接管状态
		}
		runtime := c.runtime[id]
		if runtime.Blocked || runtime.BlockedUntil != nil {
			continue // 旧式人工覆盖仍在生效
		}
		c.runtime[id] = ruleRuntime{}
	}
}
