package main

import (
	"fmt"
	"time"
)

type ModeInfo struct {
	Why         string
	Description string
}

type Mode interface {
	Run() <-chan struct{}
	Reload() <-chan struct{}
	Info() ModeInfo
}

// TimerMode covers both "for" and "until" modes.
type TimerMode struct {
	stateID int
	timer   *time.Timer
	endTime time.Time
}

func NewTimerMode(id int, endTime time.Time) *TimerMode {
	return &TimerMode{
		stateID: id,
		endTime: endTime,
	}
}

func (m *TimerMode) Run() <-chan struct{} {
	ch := make(chan struct{})
	m.timer = time.NewTimer(time.Until(m.endTime))
	go func() {
		<-m.timer.C
		close(ch)
	}()
	return ch
}

func (m *TimerMode) Reload() <-chan struct{} {
	s, err := readState(m.stateID)
	if err != nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	m.endTime = s.EndTime
	if m.timer != nil {
		m.timer.Stop()
	}
	return m.Run()
}

func (m *TimerMode) Info() ModeInfo {
	return ModeInfo{
		Why:         fmt.Sprintf("block-sleep until %s", m.endTime.Format("15:04")),
		Description: fmt.Sprintf("until %s", m.endTime.Format("15:04")),
	}
}
