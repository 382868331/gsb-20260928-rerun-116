package smarthome

import (
	"encoding/json"
	"os"
)

// valueDTO 是 Value 的可序列化形式。
type valueDTO struct {
	Kind byte    `json:"kind"` // 'n' / 'b' / 's'
	Num  float64 `json:"num,omitempty"`
	Bl   bool    `json:"bl,omitempty"`
	Str  string  `json:"str,omitempty"`
}

func toDTO(v Value) valueDTO {
	return valueDTO{Kind: v.kind, Num: v.num, Bl: v.bl, Str: v.str}
}

func fromDTO(d valueDTO) Value {
	return Value{kind: d.Kind, num: d.Num, bl: d.Bl, str: d.Str}
}

// MarshalSnapshot 导出引擎全部需要在重启后继续的状态：
// 当前时刻、持续计时、未完成动作、手动接管锁与各设备属性值。
func (e *Engine) MarshalSnapshot() ([]byte, error) {
	snap := Snapshot{
		Time:      e.clock.Now(),
		Waiting:   map[string]waitDTO{},
		Pending:   make([]PendingStep, 0, len(e.pending)),
		Overrides: make([]overrideKey, 0, len(e.overrides)),
		Devices:   map[string]deviceSnap{},
	}
	for id, w := range e.waiting {
		snap.Waiting[id] = waitDTO{DueAt: w.dueAt, Fired: w.fired}
	}
	for _, p := range e.pending {
		snap.Pending = append(snap.Pending, *p)
	}
	for k := range e.overrides {
		snap.Overrides = append(snap.Overrides, k)
	}
	for id, d := range e.devices {
		st := map[string]valueDTO{}
		for prop, v := range d.Snapshot() {
			st[prop] = toDTO(v)
		}
		snap.Devices[id] = deviceSnap{State: st}
	}
	return json.MarshalIndent(snap, "", "  ")
}

// UnmarshalSnapshot 从快照恢复。设备需先以相同 ID/属性定义注册，
// 规则也需先重新加载；恢复后计时与未完成动作按绝对时间继续。
func (e *Engine) UnmarshalSnapshot(data []byte) error {
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}
	// 恢复设备属性（直接改状态，不重新发事件，避免重启瞬间触发级联）。
	for id, ds := range snap.Devices {
		d, ok := e.devices[id].(*VirtualDevice)
		if !ok {
			continue
		}
		for prop, dto := range ds.State {
			d.state[prop] = fromDTO(dto)
		}
	}
	e.waiting = map[string]*waiting{}
	for id, wd := range snap.Waiting {
		if _, ok := e.rules[id]; !ok {
			continue
		}
		e.waiting[id] = &waiting{dueAt: wd.DueAt, fired: wd.Fired}
	}
	e.pending = nil
	for i := range snap.Pending {
		p := snap.Pending[i]
		e.pending = append(e.pending, &p)
	}
	e.overrides = map[overrideKey]struct{}{}
	for _, k := range snap.Overrides {
		e.overrides[k] = struct{}{}
	}
	// 恢复后立即把已到期的未完成动作执行掉，其余继续等待。
	e.runDue(snap.Time, nil)
	return nil
}

// SaveToFile 把快照写入文件（原子替换）。
func (e *Engine) SaveToFile(path string) error {
	data, err := e.MarshalSnapshot()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadFromFile 从文件恢复；文件不存在时返回 nil（首次启动）。
func (e *Engine) LoadFromFile(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return e.UnmarshalSnapshot(data)
}
