package automation

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type Store interface {
	Load() (*persistentState, error)
	Save(persistentState) error
}

type FileStore struct {
	Path string
}

func (s FileStore) Load() (*persistentState, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state persistentState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("load automation state: %w", err)
	}
	return &state, nil
}

func (s FileStore) Save(state persistentState) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	temporary := s.Path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, s.Path)
}
