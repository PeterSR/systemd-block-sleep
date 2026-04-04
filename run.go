package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func startRun(cmdArgs []string, what string) {
	if err := ensureStateDir(); err != nil {
		fatalf("Failed to create state directory: %v", err)
	}

	id, err := nextID()
	if err != nil {
		fatalf("Failed to allocate block ID: %v", err)
	}

	state := &State{
		ID:      id,
		PID:     os.Getpid(),
		What:    what,
		Mode:    "run",
		Started: time.Now(),
		While: &WhileInfo{
			Type:  "run",
			Value: strings.Join(cmdArgs, " "),
		},
	}

	// Create FIFO for systemd-inhibit
	fp := blockFifoPath(id)
	os.Remove(fp)
	if err := syscall.Mkfifo(fp, 0644); err != nil {
		fatalf("Failed to create FIFO: %v", err)
	}
	defer os.Remove(fp)

	// Start systemd-inhibit
	why := fmt.Sprintf("block-sleep run %s", strings.Join(cmdArgs, " "))
	inhibitCmd := exec.Command("sudo", "systemd-inhibit",
		"--what="+what, "--why="+why, "--mode=block",
		"cat", fp)
	if err := inhibitCmd.Start(); err != nil {
		fatalf("Failed to start systemd-inhibit: %v", err)
	}
	defer inhibitCmd.Process.Kill()
	defer inhibitCmd.Wait()

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

	// Write state file
	if err := writeState(state); err != nil {
		fatalf("Failed to write state: %v", err)
	}
	defer removeState(id)

	fmt.Printf("Sleep blocked while running: %s [#%d]\n", strings.Join(cmdArgs, " "), id)

	// Forward SIGINT/SIGTERM to child
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Run the user's command with stdio passthrough
	userCmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	userCmd.Stdin = os.Stdin
	userCmd.Stdout = os.Stdout
	userCmd.Stderr = os.Stderr

	if err := userCmd.Start(); err != nil {
		fatalf("Failed to start command: %v", err)
	}

	// Forward signals to child in background
	go func() {
		for sig := range sigCh {
			userCmd.Process.Signal(sig)
		}
	}()

	err = userCmd.Wait()
	signal.Stop(sigCh)

	fmt.Printf("Block #%d released.\n", id)

	// Forward the child's exit code
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		os.Exit(1)
	}
}
