package automation

import "time"

type VirtualDevice struct {
	ID    string
	Types []string
}

func (d VirtualDevice) Event(name string, value float64, at time.Time) Event {
	return Event{
		DeviceID:   d.ID,
		DeviceType: append([]string(nil), d.Types...),
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

func ManualControl(deviceID, name string, value float64, at time.Time) Event {
	return Event{
		DeviceID: deviceID,
		Name:     name,
		Value:    value,
		Source:   SourceManual,
		Time:     at,
	}
}
