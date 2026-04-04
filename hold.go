package main

import (
	"fmt"
	"os"
	"syscall"
)

// HoldMode blocks on reading from a user-specified FIFO.
// When anything writes to the pipe, the read returns and the block releases.
type HoldMode struct {
	pipePath string
}

func NewHoldMode(pipePath string) *HoldMode {
	return &HoldMode{pipePath: pipePath}
}

func (m *HoldMode) Run() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		// Create the FIFO if it doesn't exist
		if _, err := os.Stat(m.pipePath); os.IsNotExist(err) {
			if err := syscall.Mkfifo(m.pipePath, 0644); err != nil {
				return
			}
		}
		// Block on read — returns when someone writes or pipe is closed
		f, err := os.Open(m.pipePath)
		if err != nil {
			return
		}
		defer f.Close()
		buf := make([]byte, 1)
		f.Read(buf)
	}()
	return ch
}

func (m *HoldMode) Reload() <-chan struct{} {
	return m.Run()
}

func (m *HoldMode) Info() ModeInfo {
	return ModeInfo{
		Why:         fmt.Sprintf("block-sleep hold %s", m.pipePath),
		Description: fmt.Sprintf("hold %s", m.pipePath),
	}
}

// ForeverMode never signals done. Only SIGTERM (via stop) can end it.
type ForeverMode struct{}

func NewForeverMode() *ForeverMode {
	return &ForeverMode{}
}

func (m *ForeverMode) Run() <-chan struct{} {
	// Return a channel that never closes
	return make(chan struct{})
}

func (m *ForeverMode) Reload() <-chan struct{} {
	return m.Run()
}

func (m *ForeverMode) Info() ModeInfo {
	return ModeInfo{
		Why:         "block-sleep forever",
		Description: "forever",
	}
}
