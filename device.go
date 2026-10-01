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

// Availability 上报设备可用性：online=false 表示设备离线，
// 离线期间其属性对条件不可见，指向它的延迟动作到点时取消。
func (d VirtualDevice) Availability(online bool, at time.Time) Event {
	value := 0.0
	if online {
		value = 1
	}
	return d.Event(Online, value, at)
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

func ManualControl(deviceID, name string, value float64, at time.Time) Event {
	return Event{
		DeviceID: deviceID,
		Name:     name,
		Value:    value,
		Source:   SourceManual,
		Time:     at,
	}
}
