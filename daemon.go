package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func runDaemon(id int) {
	state, err := readState(id)
	if err != nil {
		fatalf("Failed to read state: %v", err)
	}

	var mode Mode
	switch state.Mode {
	case "for", "until":
		mode = NewTimerMode(id, state.EndTime)
	case "await":
		if state.While == nil {
			fatalf("Await mode missing condition info")
		}
		switch state.While.Type {
		case "pid":
			pid, _ := strconv.Atoi(state.While.Value)
			mode = NewAwaitPIDMode(pid)
		case "cmd":
			interval := defaultPollInterval
			if state.While.Interval != "" {
				if d, err := time.ParseDuration(state.While.Interval); err == nil {
					interval = d
				}
			}
			mode = NewAwaitCmdMode(state.While.Value, interval)
		default:
			fatalf("Unknown await type: %s", state.While.Type)
		}
	case "hold":
		if state.While == nil || state.While.Value == "" {
			fatalf("Hold mode missing pipe path")
		}
		mode = NewHoldMode(state.While.Value)
	case "forever":
		mode = NewForeverMode()
	default:
		fatalf("Unknown mode: %s", state.Mode)
	}

	// Create FIFO for systemd-inhibit
	fp := blockFifoPath(id)
	os.Remove(fp)
	if err := syscall.Mkfifo(fp, 0644); err != nil {
		fatalf("Failed to create FIFO: %v", err)
	}
	defer os.Remove(fp)

	// Start systemd-inhibit with cat reading from the FIFO
	why := mode.Info().Why
	cmd := exec.Command("sudo", "systemd-inhibit",
		"--what="+state.What, "--why="+why, "--mode=block",
		"cat", fp)
	if err := cmd.Start(); err != nil {
		fatalf("Failed to start systemd-inhibit: %v", err)
	}
	defer cmd.Process.Kill()
	defer cmd.Wait()

	// Open FIFO for writing — blocks until cat opens the read end
	type openResult struct {
		f   *os.File
		err error
	}
	ch := make(chan openResult, 1)
	go func() {
		f, err := os.OpenFile(fp, os.O_WRONLY, 0)
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

	// Update PID in state file to signal readiness to client
	state.PID = os.Getpid()
	if err := writeState(state); err != nil {
		fatalf("Failed to write state: %v", err)
	}
	defer removeState(id)

	// Run mode + signal handling — no polling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGUSR1)

	doneCh := mode.Run()

	for {
		select {
		case <-doneCh:
			return
		case sig := <-sigCh:
			if sig == syscall.SIGTERM {
				return
			}
			// SIGUSR1: reload state (for extend)
			doneCh = mode.Reload()
		}
	}
}

func startBlock(state *State) {
	if err := ensureStateDir(); err != nil {
		fatalf("Failed to create state directory: %v", err)
	}

	id, err := nextID()
	if err != nil {
		fatalf("Failed to allocate block ID: %v", err)
	}
	state.ID = id
	state.PID = 0
	state.Started = time.Now()

	if err := writeState(state); err != nil {
		fatalf("Failed to write state: %v", err)
	}

	exe, err := os.Executable()
	if err != nil {
		removeState(id)
		fatalf("Failed to find executable: %v", err)
	}

	cmd := exec.Command(exe, "_daemon", fmt.Sprintf("%d", id))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		removeState(id)
		fatalf("Failed to open /dev/null: %v", err)
	}
	defer devNull.Close()
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.Stdin = devNull

	if err := cmd.Start(); err != nil {
		removeState(id)
		fatalf("Failed to start daemon: %v", err)
	}
	go cmd.Wait()

	// Wait for daemon to signal readiness via PID in state file
	for range 50 {
		if s, err := readState(id); err == nil && s.PID > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	removeState(id)
	fatalf("Failed to start sleep block. Is sudo configured? Try: block-sleep install-sudoers")
}
