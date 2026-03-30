package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const defaultPollInterval = 30 * time.Second

// AwaitPIDMode watches a process via pidfd_open (Linux 5.3+).
// Falls back to polling /proc/<pid> if pidfd is unavailable.
type AwaitPIDMode struct {
	pid int
}

func NewAwaitPIDMode(pid int) *AwaitPIDMode {
	return &AwaitPIDMode{pid: pid}
}

func (m *AwaitPIDMode) Run() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		// Try pidfd_open first (event-driven, no polling)
		fd, err := pidfdOpen(m.pid)
		if err == nil {
			defer syscall.Close(fd)
			pidfdWait(fd)
			return
		}
		// Fallback: poll /proc/<pid>
		for processExists(m.pid) {
			time.Sleep(1 * time.Second)
		}
	}()
	return ch
}

func (m *AwaitPIDMode) Reload() <-chan struct{} {
	return m.Run()
}

func (m *AwaitPIDMode) Info() ModeInfo {
	return ModeInfo{
		Why:         fmt.Sprintf("block-sleep await pid %d", m.pid),
		Description: fmt.Sprintf("await pid %d", m.pid),
	}
}

// AwaitCmdMode polls a shell command at an interval.
// Block continues while the command exits 0.
type AwaitCmdMode struct {
	cmd      string
	interval time.Duration
}

func NewAwaitCmdMode(cmd string, interval time.Duration) *AwaitCmdMode {
	return &AwaitCmdMode{cmd: cmd, interval: interval}
}

func (m *AwaitCmdMode) Run() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		for {
			err := exec.Command("sh", "-c", m.cmd).Run()
			if err != nil {
				return // command failed, condition no longer true
			}
			time.Sleep(m.interval)
		}
	}()
	return ch
}

func (m *AwaitCmdMode) Reload() <-chan struct{} {
	return m.Run()
}

func (m *AwaitCmdMode) Info() ModeInfo {
	return ModeInfo{
		Why:         fmt.Sprintf("block-sleep await %q", m.cmd),
		Description: fmt.Sprintf("await cmd %q", m.cmd),
	}
}

// pidfd_open syscall (Linux 5.3+, syscall number 434 on amd64)
func pidfdOpen(pid int) (int, error) {
	fd, _, errno := syscall.Syscall(434, uintptr(pid), 0, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(fd), nil
}

// pidfdWait blocks until the process exits by polling the pidfd.
func pidfdWait(fd int) {
	pollfd := struct {
		fd      int32
		events  int16
		revents int16
	}{
		fd:     int32(fd),
		events: 0x0001, // POLLIN
	}
	for {
		n, _, errno := syscall.Syscall(syscall.SYS_POLL, uintptr(unsafe.Pointer(&pollfd)), 1, uintptr(^uint(0)>>1))
		if n > 0 {
			return
		}
		if errno != 0 && errno != syscall.EINTR {
			return
		}
	}
}

func startAwait(target string, every time.Duration, what string) {
	state := &State{
		What: what,
		Mode: "await",
	}

	// Auto-detect: number = PID, otherwise = command
	if pid, err := strconv.Atoi(target); err == nil {
		if !processExists(pid) {
			fatalf("Process %d does not exist.", pid)
		}
		state.While = &WhileInfo{
			Type:  "pid",
			Value: target,
		}
	} else {
		state.While = &WhileInfo{
			Type:     "cmd",
			Value:    target,
			Interval: every.String(),
		}
	}

	startBlock(state)

	switch state.While.Type {
	case "pid":
		fmt.Printf("Sleep blocked while PID %s is alive [#%d]\n", state.While.Value, state.ID)
	case "cmd":
		fmt.Printf("Sleep blocked while %q succeeds (every %s) [#%d]\n", state.While.Value, every, state.ID)
	}
}

func parseAwaitArgs(args []string) (target string, every time.Duration) {
	every = defaultPollInterval
	var positional []string

	for _, a := range args {
		if strings.HasPrefix(a, "--every=") {
			v := strings.TrimPrefix(a, "--every=")
			d, err := time.ParseDuration(v)
			if err != nil {
				fatalf("Invalid interval: %s", v)
			}
			every = d
		} else {
			positional = append(positional, a)
		}
	}

	if len(positional) != 1 {
		fatalf("Usage: block-sleep await <pid-or-command> [--every=<interval>]")
	}
	target = positional[0]
	return
}
