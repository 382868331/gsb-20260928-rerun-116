package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"automation"
)

// writeHistory 把事件历史落盘，供 --replay 离线重放。
func writeHistory(path string, history []automation.Event) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func readHistory(path string) ([]automation.Event, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var history []automation.Event
	if err := json.Unmarshal(data, &history); err != nil {
		return nil, err
	}
	return history, nil
}
