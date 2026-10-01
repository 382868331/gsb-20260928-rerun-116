package automation

import "time"

type VirtualDevice struct {
	ID    string
	Types []string
	Room  string
}

func (d VirtualDevice) Event(name string, value float64, at time.Time) Event {
	return Event{
		DeviceID:   d.ID,
		DeviceType: append([]string(nil), d.Types...),
		Room:       d.Room,
		Name:       name,
		Value:      value,
		Source:     SourceDevice,
		Time:       at,
	}
}

func (d VirtualDevice) Contact(open bool, at time.Time) Event {
	value := 0.0
	if open {
		value = 1
	}
	return d.Event(ContactOpen, value, at)
}

func (d VirtualDevice) Temperature(celsius float64, at time.Time) Event {
	return d.Event(TemperatureC, celsius, at)
}

// Status 上报设备在线状态（true 在线，false 离线）。
// 离线设备上的自动化动作到点时不会落地，决策日志记录 device_offline。
func (d VirtualDevice) Status(online bool, at time.Time) Event {
	value := 0.0
	if online {
		value = 1
	}
	return d.Event(Online, value, at)
}

func ManualControl(deviceID, name string, value float64, at time.Time) Event {
	return Event{
		DeviceID: deviceID,
		Name:     name,
		Value:    value,
		Source:   SourceManual,
		Time:     at,
	}
}
