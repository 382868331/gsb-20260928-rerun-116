package smarthome

import (
	"fmt"
	"sort"
)

// maxCascade 限制同一条自动因果链的长度。规则 A 引起的状态又满足 B，
// B 再引起状态满足 A —— 这种环会被 Trace 直接切断，该值是最后的硬上限。
const maxCascade = 64

// maxActionsPerPump 限制一次事件处理中动作步骤总数，属于防御性上限。
const maxActionsPerPump = 10000

// waiting 记录一条“条件已成立、正在持续计时”的规则。
type waiting struct {
	dueAt Duration // 条件持续成立到该时刻即触发
	fired bool     // 本个“真周期”内已触发，需等条件变假才能再次触发
}

// PendingStep 是一个尚未到执行时刻的动作步骤，可序列化落盘。
type PendingStep struct {
	ActionID string
	RuleID   string
	Seq      int      // 第几步
	DueAt    Duration // 绝对引擎时间
	Device   string
	Prop     string
	Value    valueDTO
	Trace    []string // 触发该动作时的因果链
}

// Snapshot 是引擎可持久化的全部运行时状态。
type Snapshot struct {
	Time      Duration              `json:"time"`
	Waiting   map[string]waitDTO    `json:"waiting"` // 持续计时
	Pending   []PendingStep         `json:"pending"` // 未完成动作
	Overrides []overrideKey         `json:"overrides"`
	Devices   map[string]deviceSnap `json:"devices"`
}

type waitDTO struct {
	DueAt Duration `json:"due_at"`
	Fired bool     `json:"fired"`
}

type overrideKey struct {
	Device string `json:"device"`
	Prop   string `json:"prop"`
}

type deviceSnap struct {
	State map[string]valueDTO `json:"state"`
}

// Engine 是自动化核心。
type Engine struct {
	bus   *Bus
	clock Clock

	devices map[string]Device
	rules   map[string]*Rule
	ruleIDs []string // 保持确定性的评估顺序

	waiting   map[string]*waiting
	pending   []*PendingStep
	overrides map[overrideKey]struct{}

	// stats 便于测试与演示观察。
	triggered  int
	suppressed int
	blocked    int
}

func NewEngine(bus *Bus, clock Clock) *Engine {
	e := &Engine{
		bus:       bus,
		clock:     clock,
		devices:   map[string]Device{},
		rules:     map[string]*Rule{},
		waiting:   map[string]*waiting{},
		overrides: map[overrideKey]struct{}{},
	}
	bus.Subscribe(e.onEvent)
	return e
}

func (e *Engine) AddDevice(d Device) {
	if _, exists := e.devices[d.ID()]; exists {
		return
	}
	e.devices[d.ID()] = d
}

func (e *Engine) AddRule(r *Rule) error {
	if r.ID == "" {
		return fmt.Errorf("rule id required")
	}
	if _, exists := e.rules[r.ID]; exists {
		return fmt.Errorf("rule %q duplicated", r.ID)
	}
	if len(r.All) == 0 {
		return fmt.Errorf("rule %q needs at least one condition", r.ID)
	}
	if len(r.Action.Steps) == 0 {
		return fmt.Errorf("rule %q needs at least one action step", r.ID)
	}
	e.rules[r.ID] = r
	e.ruleIDs = append(e.ruleIDs, r.ID)
	sort.Strings(e.ruleIDs)
	e.evaluate(e.clock.Now(), nil)
	return nil
}

// Device 暴露设备句柄，供手动操作/演示使用。
func (e *Engine) Device(id string) Device { return e.devices[id] }

// ManualSet 表示人直接操作设备：立即生效并对该目标建立“接管锁”，
// 之后的自动动作不能立刻覆盖，直到调用 ReleaseOverride 或规则条件解除。
func (e *Engine) ManualSet(deviceID, prop string, v Value) error {
	d, ok := e.devices[deviceID].(*VirtualDevice)
	if !ok {
		return fmt.Errorf("device %q not found", deviceID)
	}
	k := overrideKey{deviceID, prop}
	e.overrides[k] = struct{}{}
	d.change(prop, v, SourceManual, nil)
	// 手动改变可能使某些条件不再成立，立即评估以清理计时。
	e.evaluate(e.clock.Now(), nil)
	e.runDue(e.clock.Now(), nil)
	return nil
}

// ReleaseOverride 解除手动接管；下一次条件“假→真并持续”时规则才能再次动作。
func (e *Engine) ReleaseOverride(deviceID, prop string) {
	delete(e.overrides, overrideKey{deviceID, prop})
}

// Overridden 报告目标是否处于手动接管中。
func (e *Engine) Overridden(deviceID, prop string) bool {
	_, ok := e.overrides[overrideKey{deviceID, prop}]
	return ok
}

// onEvent 是总线回调：任何设备状态变化都重新评估持续条件。
func (e *Engine) onEvent(ev Event) {
	now := e.clock.Now()
	e.evaluate(now, ev.Trace)
	// system/manual 事件开启全新的因果链（trace 为空）；
	// auto 事件沿用该动作携带的链，防环依赖它。
	e.runDue(now, ev.Trace)
}

// evaluate 按统一含义检查全部规则的条件，并维护持续计时器。
func (e *Engine) evaluate(now Duration, trace []string) {
	ids := append([]string(nil), e.ruleIDs...)
	sort.Strings(ids)
	for _, id := range ids {
		r := e.rules[id]
		ok, err := r.satisfied(e.devices)
		if err != nil || !ok {
			// 条件被破坏（或暂时无法判断）：计时作废，回到待触发态。
			delete(e.waiting, id)
			continue
		}
		w, pending := e.waiting[id]
		if !pending {
			// 假→真边沿：开始计时。SustainFor==0 表示立即满足。
			w = &waiting{dueAt: now + r.SustainFor}
			e.waiting[id] = w
			if r.SustainFor == 0 {
				w.fired = true
				e.fire(r, now, trace)
			}
		} else if now >= w.dueAt {
			if !w.fired {
				// 持续满足到期：本个真周期只触发一次。
				w.fired = true
				e.fire(r, w.dueAt, trace)
			}
		}
	}
}

// fire 把规则动作的全部步骤按各自 Delay 调度成“未完成动作”。
func (e *Engine) fire(r *Rule, at Duration, trace []string) {
	e.triggered++
	// 步骤携带“祖先规则链”：本规则由这些祖先动作级联而来。
	// 当该步骤引起的新事件再次触发本规则时，execute 会在链中发现它，从而断环。
	chain := append([]string(nil), trace...)
	t := at
	for i, s := range r.Action.Steps {
		t += s.Delay
		e.pending = append(e.pending, &PendingStep{
			ActionID: r.Action.ID,
			RuleID:   r.ID,
			Seq:      i,
			DueAt:    t,
			Device:   s.Device,
			Prop:     s.Prop,
			Value:    toDTO(s.Value),
			Trace:    append([]string(nil), chain...),
		})
	}
}

// runDue 执行所有到期动作步骤，返回执行条数。
func (e *Engine) runDue(now Duration, currentTrace []string) int {
	executed := 0
	for executed < maxActionsPerPump {
		idx := e.nextDueIndex(now)
		if idx < 0 {
			break
		}
		p := e.pending[idx]
		e.pending = append(e.pending[:idx], e.pending[idx+1:]...)
		e.execute(p, currentTrace)
		executed++
	}
	return executed
}

func (e *Engine) nextDueIndex(now Duration) int {
	idx := -1
	for i, p := range e.pending {
		if p.DueAt <= now && (idx == -1 || p.DueAt < e.pending[idx].DueAt) {
			idx = i
		}
	}
	return idx
}

func (e *Engine) execute(p *PendingStep, currentTrace []string) {
	d, ok := e.devices[p.Device].(*VirtualDevice)
	if !ok {
		e.blocked++
		return
	}
	// 防触发环：本步骤所属规则已在当前自动因果链中出现过则丢弃。
	for _, id := range p.Trace {
		if id == p.RuleID {
			e.blocked++
			return
		}
	}
	if len(p.Trace) >= maxCascade {
		e.blocked++
		return
	}
	v := fromDTO(p.Value)
	// 手动接管锁：人操作过的目标，自动动作不能覆盖。
	if _, taken := e.overrides[overrideKey{p.Device, p.Prop}]; taken {
		e.suppressed++
		return
	}
	if !d.Writable(p.Prop) {
		e.blocked++
		return
	}
	// 本动作引发的后续规则，其因果链应为“祖先 + 本规则”。
	childTrace := append(append([]string(nil), p.Trace...), p.RuleID)
	d.change(p.Prop, v, SourceAuto, childTrace)
}

// Tick 推进时间：驱动持续计时与到期动作。FakeClock 配合测试使用；
// WallClock 下由 Run 周期性调用。
func (e *Engine) Tick(now Duration) {
	e.evaluate(now, nil)
	// Tick 属于系统唤醒，不携带正在处理的某条事件的因果链。
	e.runDue(now, nil)
}

// Stats 是可观测计数的快照。
type Stats struct {
	Triggered  int
	Suppressed int // 被手动接管挡下的步骤
	Blocked    int // 因防环/只读/设备缺失未执行的步骤
}

func (e *Engine) Stats() Stats {
	return Stats{Triggered: e.triggered, Suppressed: e.suppressed, Blocked: e.blocked}
}

func (e *Engine) PendingCount() int { return len(e.pending) }

func (e *Engine) WaitingRuleCount() int { return len(e.waiting) }
