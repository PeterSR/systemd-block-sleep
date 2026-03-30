package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type WhileInfo struct {
	Type     string `json:"type"`
	Value    string `json:"value"`
	Interval string `json:"interval,omitempty"`
}

type State struct {
	ID      int        `json:"id"`
	PID     int        `json:"pid"`
	What    string     `json:"what"`
	Mode    string     `json:"mode"`
	EndTime time.Time  `json:"end,omitempty"`
	While   *WhileInfo `json:"while,omitempty"`
	Started time.Time  `json:"started"`
}

func stateDir() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "block-sleep")
	}
	return "/tmp/block-sleep"
}

func ensureStateDir() error {
	return os.MkdirAll(stateDir(), 0755)
}

func statePath(id int) string {
	return filepath.Join(stateDir(), fmt.Sprintf("%d.json", id))
}

func blockFifoPath(id int) string {
	return filepath.Join(stateDir(), fmt.Sprintf("%d.pipe", id))
}

func readState(id int) (*State, error) {
	data, err := os.ReadFile(statePath(id))
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func writeState(s *State) error {
	if err := ensureStateDir(); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(s.ID), data, 0644)
}

func removeState(id int) {
	os.Remove(statePath(id))
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

func isBlockActive(s *State) bool {
	return s.PID > 0 && processExists(s.PID)
}

func allStates() ([]*State, error) {
	dir := stateDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var states []*State
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		idStr := strings.TrimSuffix(e.Name(), ".json")
		id, err := strconv.Atoi(idStr)
		if err != nil {
			continue
		}
		s, err := readState(id)
		if err != nil {
			continue
		}
		states = append(states, s)
	}

	sort.Slice(states, func(i, j int) bool {
		return states[i].ID < states[j].ID
	})
	return states, nil
}

func activeStates() ([]*State, error) {
	all, err := allStates()
	if err != nil {
		return nil, err
	}
	var active []*State
	for _, s := range all {
		if isBlockActive(s) {
			active = append(active, s)
		} else {
			removeState(s.ID)
			os.Remove(blockFifoPath(s.ID))
		}
	}
	return active, nil
}

func nextID() (int, error) {
	all, err := allStates()
	if err != nil {
		return 0, err
	}
	maxID := 0
	for _, s := range all {
		if s.ID > maxID {
			maxID = s.ID
		}
	}
	return maxID + 1, nil
}
