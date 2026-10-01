package automation

import (
	"fmt"
	"sort"
	"time"
)

// BeginTakeover 建立一层人工接管，返回接管层 ID。
// 接管以事件形式进入历史，因此重启恢复与离线重放都会复现同一层接管。
// 多层接管（例如“整个房间接管”+“某设备临时例外”）相互独立叠加：
// 每层有自己的作用范围与 TTL，到期或撤销一层只解除该层的约束。
func (c *Core) BeginTakeover(spec Takeover, at time.Time) (string, error) {
	if spec.Room == "" && spec.DeviceID == "" {
		return "", fmt.Errorf("takeover requires a room or device scope")
	}
	if at.IsZero() {
		at = c.now()
	}
	c.mu.Lock()
	c.takeoverSeq++
	if spec.ID == "" {
		spec.ID = fmt.Sprintf("takeover-%d", c.takeoverSeq)
	}
	id := spec.ID
	c.mu.Unlock()
	event := Event{
		DeviceID: spec.DeviceID,
		Name:     EventTakeoverBegin,
		Source:   SourceManual,
		Time:     at,
		Takeover: &spec,
	}
	if _, err := c.Process(event); err != nil {
		return "", err
	}
	return id, nil
}

// ReleaseTakeover 显式撤销一层接管。其余仍有效的接管层继续生效，
// 被释放的规则从当前时刻重新开始观察条件，而不是回到最早的自动状态。
func (c *Core) ReleaseTakeover(id string, at time.Time) error {
	if at.IsZero() {
		at = c.now()
	}
	event := Event{
		Name:     EventTakeoverEnd,
		Source:   SourceManual,
		Time:     at,
		Takeover: &Takeover{ID: id},
	}
	_, err := c.Process(event)
	return err
}

// ActiveTakeovers 返回当前仍有效的接管层（按 ID 排序）。
func (c *Core) ActiveTakeovers() []Takeover {
	c.mu.Lock()
	defer c.mu.Unlock()
	layers := make([]Takeover, 0, len(c.takeovers))
	for _, layer := range c.takeovers {
		layers = append(layers, layer)
	}
	sort.Slice(layers, func(i, j int) bool { return layers[i].ID < layers[j].ID })
	return layers
}

// beginTakeoverLocked 落地一层接管：取消被覆盖的挂起动作，
// 并让被覆盖的规则重新观察条件（保留 ManualControl 遗留的覆盖状态）。
func (c *Core) beginTakeoverLocked(spec Takeover, at time.Time) {
	layer := spec
	layer.CreatedAt = at
	if layer.TTL > 0 {
		until := at.Add(layer.TTL)
		layer.Until = &until
	}
	c.takeovers[layer.ID] = layer
	c.recordDecisionLocked(Decision{
		At: at, Outcome: OutcomeTakeoverBegin, Reason: ReasonTakeoverStarted,
		TakeoverID: layer.ID, DeviceID: layer.DeviceID, Name: layer.Name,
	})
	for _, action := range c.orderedPendingLocked() {
		if c.layerCoversTargetLocked(layer, action.DeviceID, action.Name) {
			c.cancelPendingLocked(action, at, ReasonManualIntervention)
		}
	}
	for id, rule := range c.rules {
		if c.layerCoversTargetLocked(layer, rule.Action.DeviceID, rule.Action.Name) {
			runtime := c.runtime[id]
			runtime.Satisfied = false
			runtime.Fired = false
			runtime.Since = time.Time{}
			c.runtime[id] = runtime
		}
	}
}

// endTakeoverLocked 结束一层接管（到期或撤销）。只移除该层，
// 其余接管层继续生效；不再被任何层覆盖的规则自然地从零开始重新观察。
func (c *Core) endTakeoverLocked(id string, at time.Time, reason Reason) {
	layer, exists := c.takeovers[id]
	if !exists {
		return
	}
	delete(c.takeovers, id)
	c.recordDecisionLocked(Decision{
		At: at, Outcome: OutcomeTakeoverEnd, Reason: reason,
		TakeoverID: layer.ID, DeviceID: layer.DeviceID, Name: layer.Name,
	})
}

// expireTakeoversLocked 解除已到期的接管层，返回是否有变化。
func (c *Core) expireTakeoversLocked(at time.Time) bool {
	changed := false
	for id, layer := range c.takeovers {
		if layer.Until != nil && !layer.Until.After(at) {
			c.endTakeoverLocked(id, at, ReasonTakeoverExpired)
			changed = true
		}
	}
	return changed
}

// layerCoversTargetLocked 判断接管层是否覆盖某设备属性。
func (c *Core) layerCoversTargetLocked(layer Takeover, deviceID, name string) bool {
	if layer.DeviceID != "" {
		if layer.DeviceID != deviceID {
			return false
		}
		return layer.Name == "" || layer.Name == name
	}
	if layer.Room != "" {
		return c.devices[deviceID].Room == layer.Room
	}
	return false
}

// isRuleBlockedLocked 判断规则当前是否被人工接管：
// 既包括 ManualControl 建立的规则级覆盖，也包括任何仍有效的接管层。
func (c *Core) isRuleBlockedLocked(rule Rule, at time.Time) bool {
	if c.isBlockedLocked(c.runtime[rule.ID], at) {
		return true
	}
	for _, layer := range c.takeovers {
		if layer.Until != nil && !layer.Until.After(at) {
			continue
		}
		if c.layerCoversTargetLocked(layer, rule.Action.DeviceID, rule.Action.Name) {
			return true
		}
	}
	return false
}
