package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

var (
	stateFile = runtimePath("block-sleep.json")
	fifoPath  = runtimePath("block-sleep.pipe")
)

func runtimePath(name string) string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return dir + "/" + name
	}
	return "/tmp/" + name
}

type State struct {
	PID     int       `json:"pid"`
	EndTime time.Time `json:"end"`
}

func readState() (*State, error) {
	data, err := os.ReadFile(stateFile)
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
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(stateFile, data, 0644)
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

func isActive() (*State, bool) {
	s, err := readState()
	if err != nil {
		return nil, false
	}
	if !processExists(s.PID) {
		return s, false
	}
	return s, true
}

func start(d time.Duration) {
	if s, active := isActive(); active {
		rem := time.Until(s.EndTime)
		fatalf("Already blocking (PID %d, %s remaining). Use 'extend' to change or 'stop' to cancel.", s.PID, formatDuration(rem))
	}

	endTime := time.Now().Add(d)

	exe, err := os.Executable()
	if err != nil {
		fatalf("Failed to find executable: %v", err)
	}

	cmd := exec.Command(exe, "_daemon", endTime.Format(time.RFC3339Nano))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		fatalf("Failed to open /dev/null: %v", err)
	}
	defer devNull.Close()
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.Stdin = devNull

	if err := cmd.Start(); err != nil {
		fatalf("Failed to start daemon: %v", err)
	}
	go cmd.Wait()

	// Wait for daemon to signal readiness via state file
	for range 50 {
		if s, err := readState(); err == nil && s.PID > 0 {
			fmt.Printf("Sleep blocked for %s (until %s)\n", formatDuration(d), endTime.Format("15:04"))
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	fatalf("Failed to start sleep block. Is sudo configured? Try: block-sleep install-sudoers")
}

// runDaemon is the long-lived user-space process that holds the inhibit lock.
// It creates a FIFO and runs sudo systemd-inhibit ... cat <fifo>. The inhibit
// lock is held as long as cat blocks on the FIFO. When the daemon exits, the
// FIFO write-end closes, cat gets EOF, and systemd-inhibit releases the lock.
//
// The daemon blocks in select{timer, signal} — no polling, no CPU.
// Extend sends SIGUSR1, stop sends SIGTERM.
func runDaemon(endTimeStr string) {
	endTime, err := time.Parse(time.RFC3339Nano, endTimeStr)
	if err != nil {
		fatalf("Invalid end time: %v", err)
	}

	// Create FIFO
	os.Remove(fifoPath)
	if err := syscall.Mkfifo(fifoPath, 0644); err != nil {
		fatalf("Failed to create FIFO: %v", err)
	}
	defer os.Remove(fifoPath)

	// Start systemd-inhibit with cat reading from the FIFO
	why := fmt.Sprintf("block-sleep until %s", endTime.Format("15:04"))
	cmd := exec.Command("sudo", "systemd-inhibit",
		"--what=sleep", "--why="+why, "--mode=block",
		"cat", fifoPath)
	if err := cmd.Start(); err != nil {
		fatalf("Failed to start systemd-inhibit: %v", err)
	}
	defer cmd.Process.Kill()
	defer cmd.Wait()

	// Open FIFO for writing. This blocks until cat opens the read end.
	// Use a goroutine with timeout to avoid hanging if sudo fails.
	type openResult struct {
		f   *os.File
		err error
	}
	ch := make(chan openResult, 1)
	go func() {
		f, err := os.OpenFile(fifoPath, os.O_WRONLY, 0)
		ch <- openResult{f, err}
	}()

	var fifo *os.File
	select {
	case r := <-ch:
		if r.err != nil {
			fatalf("Failed to open FIFO: %v", r.err)
		}
		fifo = r.f
	case <-time.After(5 * time.Second):
		fatalf("Timed out waiting for systemd-inhibit to start")
	}
	defer fifo.Close()

	// Write state file to signal readiness
	state := &State{PID: os.Getpid(), EndTime: endTime}
	if err := writeState(state); err != nil {
		fatalf("Failed to write state: %v", err)
	}
	defer os.Remove(stateFile)

	// Block on timer + signals — no polling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGUSR1)

	timer := time.NewTimer(time.Until(endTime))
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			return
		case sig := <-sigCh:
			if sig == syscall.SIGTERM {
				return
			}
			// SIGUSR1: re-read state for updated end time
			s, err := readState()
			if err != nil {
				return
			}
			state.EndTime = s.EndTime
			timer.Reset(time.Until(s.EndTime))
		}
	}
}

func showStatus() {
	s, active := isActive()
	if !active {
		fatalf("No active sleep block.")
	}

	rem := time.Until(s.EndTime)
	if rem <= 0 {
		fmt.Println("Sleep block is expiring...")
		return
	}

	fmt.Printf("%s remaining (until %s)\n", formatDuration(rem), s.EndTime.Format("15:04"))
}

func extend(d time.Duration) {
	s, active := isActive()
	if !active {
		fatalf("No active sleep block.")
	}

	s.EndTime = time.Now().Add(d)
	if err := writeState(s); err != nil {
		fatalf("Failed to update state: %v", err)
	}

	// Wake the daemon to re-read the new end time
	syscall.Kill(s.PID, syscall.SIGUSR1)

	fmt.Printf("Sleep block reset to %s (until %s)\n", formatDuration(d), s.EndTime.Format("15:04"))
}

func stop() {
	s, err := readState()
	if err != nil {
		fmt.Println("No active sleep block.")
		return
	}

	if !processExists(s.PID) {
		os.Remove(stateFile)
		fmt.Println("No active sleep block (cleaned up stale state).")
		return
	}

	// SIGTERM the daemon (user process, no sudo needed).
	// Daemon exits, FIFO closes, cat gets EOF, systemd-inhibit releases lock.
	syscall.Kill(s.PID, syscall.SIGTERM)

	for range 50 {
		if !processExists(s.PID) {
			fmt.Println("Sleep block stopped.")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	fmt.Println("Sleep block stopped (forced).")
}
