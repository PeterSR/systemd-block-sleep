package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultWhat = "sleep"
)

func main() {
	args := os.Args[1:]

	// Extract global flags, but stop at "--" (used by "run" mode)
	what := ""
	configFile := ""
	var remaining []string
	pastDash := false
	for _, a := range args {
		if a == "--" {
			pastDash = true
			remaining = append(remaining, a)
			continue
		}
		if pastDash {
			remaining = append(remaining, a)
			continue
		}
		if strings.HasPrefix(a, "--what=") {
			what = strings.TrimPrefix(a, "--what=")
		} else if strings.HasPrefix(a, "--config=") {
			configFile = strings.TrimPrefix(a, "--config=")
		} else {
			remaining = append(remaining, a)
		}
	}
	args = remaining

	if len(args) == 0 {
		cfg, err := loadConfig(configFile)
		if err != nil {
			fatalf("Failed to load config: %v", err)
		}
		if cfg == nil {
			fatalf("No command specified and no config file found.\n\nCreate a config at %s, for example:\n\n  [default]\n  mode = \"for\"\n  duration = \"1h\"\n  what = \"sleep\"\n\nOr specify a command: block-sleep help", configPath())
		}
		if what == "" {
			what = cfg.Default.What
		}
		runDefault(cfg, what)
		return
	}

	if what == "" {
		what = defaultWhat
	}

	// Resolve hidden aliases
	cmd := args[0]
	switch cmd {
	case "doing":
		cmd = "run"
	case "awaiting":
		cmd = "await"
	case "holding":
		cmd = "hold"
	}

	switch cmd {
	case "_daemon":
		if len(args) < 2 {
			fatalf("usage: block-sleep _daemon <id>")
		}
		id, err := strconv.Atoi(args[1])
		if err != nil {
			fatalf("Invalid daemon ID: %s", args[1])
		}
		runDaemon(id)

	case "for":
		d := 1 * time.Hour
		if len(args) > 1 {
			d = parseDuration(args[1])
		}
		startFor(d, what)

	case "until":
		if len(args) < 2 {
			fatalf("Usage: block-sleep until <time>")
		}
		timeStr := args[1]
		// Support "2026-03-30 08:00" as two separate args
		if len(args) > 2 {
			if _, err := time.Parse("2006-01-02", args[1]); err == nil {
				timeStr = args[1] + " " + args[2]
			}
		}
		t, err := parseUntilTime(timeStr)
		if err != nil {
			fatalf("Invalid time: %s\nSupported formats: HH:MM, HH:MM:SS, YYYY-MM-DDTHH:MM, YYYY-MM-DD HH:MM", timeStr)
		}
		startUntil(t, what)

	case "run":
		// Find "--" separator
		dashIdx := -1
		for i, a := range args {
			if a == "--" {
				dashIdx = i
				break
			}
		}
		if dashIdx < 0 || dashIdx+1 >= len(args) {
			fatalf("Usage: block-sleep run -- <command> [args...]")
		}
		startRun(args[dashIdx+1:], what)

	case "await":
		if len(args) < 2 {
			fatalf("Usage: block-sleep await <pid-or-command> [--every=<interval>]")
		}
		target, every := parseAwaitArgs(args[1:])
		startAwait(target, every, what)

	case "hold":
		if len(args) < 2 {
			fatalf("Usage: block-sleep hold <pipe-path>")
		}
		startHold(args[1], what)

	case "forever":
		startForever(what)

	case "status":
		showStatus()

	case "extend":
		cmdArgs := args[1:]
		var idFlag int = -1
		var filtered []string
		for _, a := range cmdArgs {
			if strings.HasPrefix(a, "--id=") {
				v := strings.TrimPrefix(a, "--id=")
				id, err := strconv.Atoi(v)
				if err != nil {
					fatalf("Invalid block ID: %s", v)
				}
				idFlag = id
			} else {
				filtered = append(filtered, a)
			}
		}
		if len(filtered) != 1 {
			fatalf("Usage: block-sleep extend <duration> [--id=<id>]")
		}
		d := parseDuration(filtered[0])
		if idFlag >= 0 {
			extendByID(idFlag, d)
		} else {
			extendAny(d)
		}

	case "list-all":
		listAll()

	case "stop":
		if len(args) > 1 {
			id, err := strconv.Atoi(args[1])
			if err != nil {
				fatalf("Invalid block ID: %s", args[1])
			}
			stopByID(id)
		} else {
			stopAll()
		}

	case "install-sudoers":
		installSudoers()

	case "-h", "--help", "help":
		usage()

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", args[0])
		usage()
		os.Exit(1)
	}
}

func runDefault(cfg *Config, what string) {
	switch cfg.Default.Mode {
	case "for":
		d := parseDuration(cfg.Default.Duration)
		startFor(d, what)
	case "until":
		t, err := parseUntilTime(cfg.Default.Until)
		if err != nil {
			fatalf("Invalid 'until' time in config: %s", cfg.Default.Until)
		}
		startUntil(t, what)
	case "await":
		if cfg.Default.Await == "" {
			fatalf("Config 'await' mode requires 'await' field.")
		}
		every := defaultPollInterval
		if cfg.Default.Every != "" {
			d, err := time.ParseDuration(cfg.Default.Every)
			if err != nil {
				fatalf("Invalid 'every' interval in config: %s", cfg.Default.Every)
			}
			every = d
		}
		startAwait(cfg.Default.Await, every, what)
	case "hold":
		if cfg.Default.Hold == "" {
			fatalf("Config 'hold' mode requires 'hold' field with pipe path.")
		}
		startHold(cfg.Default.Hold, what)
	case "forever":
		startForever(what)
	default:
		fatalf("Unknown config default mode: %q", cfg.Default.Mode)
	}
}

func startFor(d time.Duration, what string) {
	endTime := time.Now().Add(d)
	state := &State{
		What:    what,
		Mode:    "for",
		EndTime: endTime,
	}
	startBlock(state)
	fmt.Printf("Sleep blocked for %s (until %s) [#%d]\n", formatDuration(d), endTime.Format("15:04"), state.ID)
}

func startUntil(t time.Time, what string) {
	d := time.Until(t)
	if d <= 0 {
		fatalf("Time %s is in the past.", t.Format("2006-01-02 15:04"))
	}
	state := &State{
		What:    what,
		Mode:    "until",
		EndTime: t,
	}
	startBlock(state)
	if t.Day() == time.Now().Day() {
		fmt.Printf("Sleep blocked until %s (%s) [#%d]\n", t.Format("15:04"), formatDuration(d), state.ID)
	} else {
		fmt.Printf("Sleep blocked until %s (%s) [#%d]\n", t.Format("2006-01-02 15:04"), formatDuration(d), state.ID)
	}
}

func parseUntilTime(s string) (time.Time, error) {
	now := time.Now()
	loc := now.Location()

	layouts := []struct {
		layout  string
		fixDate bool // if true, use today's date (or tomorrow if past)
	}{
		{"15:04", true},
		{"15:04:05", true},
		{"2006-01-02T15:04", false},
		{"2006-01-02 15:04", false},
		{"2006-01-02T15:04:05", false},
		{"2006-01-02 15:04:05", false},
	}

	for _, l := range layouts {
		t, err := time.ParseInLocation(l.layout, s, loc)
		if err != nil {
			continue
		}
		if l.fixDate {
			t = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc)
			if !t.After(now) {
				t = t.AddDate(0, 0, 1)
			}
		}
		return t, nil
	}

	return time.Time{}, fmt.Errorf("unrecognized time format: %s", s)
}

func startHold(pipePath string, what string) {
	state := &State{
		What: what,
		Mode: "hold",
		While: &WhileInfo{
			Type:  "pipe",
			Value: pipePath,
		},
	}
	startBlock(state)
	fmt.Printf("Sleep blocked until %s receives data [#%d]\n", pipePath, state.ID)
}

func startForever(what string) {
	state := &State{
		What: what,
		Mode: "forever",
	}
	startBlock(state)
	fmt.Printf("Sleep blocked indefinitely [#%d]\nUse 'block-sleep stop' to release.\n", state.ID)
}

func showStatus() {
	states, err := activeStates()
	if err != nil {
		fatalf("Failed to read state: %v", err)
	}
	if len(states) == 0 {
		fatalf("No active sleep blocks.")
	}

	for _, s := range states {
		switch s.Mode {
		case "for", "until":
			rem := time.Until(s.EndTime)
			if rem <= 0 {
				fmt.Printf("#%d  %s (expiring...) [%s]\n", s.ID, s.Mode, s.What)
			} else {
				fmt.Printf("#%d  %s (%s remaining, until %s) [%s]\n",
					s.ID, s.Mode, formatDuration(rem), s.EndTime.Format("15:04"), s.What)
			}
		default:
			elapsed := time.Since(s.Started)
			desc := s.Mode
			if s.While != nil {
				desc = fmt.Sprintf("%s %s %s", s.Mode, s.While.Type, s.While.Value)
			}
			fmt.Printf("#%d  %s (%s elapsed) [%s]\n", s.ID, desc, formatDuration(elapsed), s.What)
		}
	}
}

func extendByID(id int, d time.Duration) {
	s, err := readState(id)
	if err != nil {
		fatalf("No block with ID #%d.", id)
	}
	if !isBlockActive(s) {
		removeState(id)
		fatalf("Block #%d is not active.", id)
	}
	if s.Mode != "for" && s.Mode != "until" {
		fatalf("Block #%d is in %q mode and cannot be extended.", id, s.Mode)
	}

	s.EndTime = time.Now().Add(d)
	if err := writeState(s); err != nil {
		fatalf("Failed to update state: %v", err)
	}

	syscall.Kill(s.PID, syscall.SIGUSR1)
	fmt.Printf("Block #%d reset to %s (until %s)\n", s.ID, formatDuration(d), s.EndTime.Format("15:04"))
}

func extendAny(d time.Duration) {
	states, err := activeStates()
	if err != nil {
		fatalf("Failed to read state: %v", err)
	}

	var timerBlocks []*State
	for _, s := range states {
		if s.Mode == "for" || s.Mode == "until" {
			timerBlocks = append(timerBlocks, s)
		}
	}

	if len(timerBlocks) == 0 {
		fatalf("No active timer blocks to extend.")
	}
	if len(timerBlocks) > 1 {
		fmt.Fprintf(os.Stderr, "Multiple timer blocks active. Use --id=<id> to specify:\n")
		for _, s := range timerBlocks {
			fmt.Fprintf(os.Stderr, "  #%d  %s (until %s)\n", s.ID, s.Mode, s.EndTime.Format("15:04"))
		}
		os.Exit(1)
	}

	extendByID(timerBlocks[0].ID, d)
}

func stopByID(id int) {
	s, err := readState(id)
	if err != nil {
		fatalf("No block with ID #%d.", id)
	}
	if !processExists(s.PID) {
		removeState(id)
		os.Remove(blockFifoPath(id))
		fatalf("No active block with ID #%d (cleaned up stale state).", id)
	}

	syscall.Kill(s.PID, syscall.SIGTERM)
	for range 50 {
		if !processExists(s.PID) {
			fmt.Printf("Block #%d stopped.\n", id)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("Block #%d stopped (forced).\n", id)
}

func stopAll() {
	states, err := activeStates()
	if err != nil {
		fatalf("Failed to read state: %v", err)
	}
	if len(states) == 0 {
		fmt.Println("No active sleep blocks.")
		return
	}

	for _, s := range states {
		syscall.Kill(s.PID, syscall.SIGTERM)
	}

	for range 50 {
		allDead := true
		for _, s := range states {
			if processExists(s.PID) {
				allDead = false
				break
			}
		}
		if allDead {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if len(states) == 1 {
		fmt.Println("Sleep block stopped.")
	} else {
		fmt.Printf("%d sleep blocks stopped.\n", len(states))
	}
}

func listAll() {
	out, err := exec.Command("systemd-inhibit", "--list").Output()
	if err != nil {
		fatalf("Failed to list inhibitors: %v", err)
	}

	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	for _, line := range lines {
		if strings.Contains(line, "block-sleep") {
			fmt.Println(line + "  ← this tool")
		} else {
			fmt.Println(line)
		}
	}
}

func usage() {
	fmt.Print(`Usage: block-sleep <command> [options]

Block system sleep using systemd-inhibit.

Modes:
  for <duration>          Block for a duration (default: 1h)
  until <time>            Block until a specific time
  run -- <cmd> [args]     Block while a command runs (foreground)
  await <pid-or-cmd>      Block while a PID lives or command succeeds
  hold <pipe-path>        Block until pipe receives data
  forever                 Block until explicit stop

Management:
  status                  Show active blocks
  extend <dur> [--id=N]   Reset a timer block to <dur> from now
  stop [id]               Stop a block (or all blocks)
  list-all                List all sleep inhibitors on the system
  install-sudoers         Install sudoers file for passwordless operation
  help                    Show this help

Global flags:
  --what=<what>           What to inhibit (default: sleep)
  --config=<path>         Config file path override

Duration formats:
  2            2 hours
  1.5          1 hour 30 minutes
  2h30m        2 hours 30 minutes
  45m          45 minutes

Time formats (for 'until'):
  22:00                   Today (or tomorrow if past)
  2026-03-30T08:00        Absolute datetime
  2026-03-30 08:00        Absolute datetime

Examples:
  block-sleep                              Run default action from config
  block-sleep for 2h                       Block for 2 hours
  block-sleep for 45m --what=sleep:idle    Also prevent screen blanking
  block-sleep until 22:00                  Block until 10 PM
  block-sleep until 2026-04-01T06:00       Block until a specific datetime
  block-sleep run -- rsync -av src/ dst/   Block while rsync runs
  block-sleep run -- make -j8 build        Exit code is forwarded
  block-sleep await $!                     Block while a background PID lives
  block-sleep await "pgrep firefox"        Block while firefox is running
  block-sleep await "curl -sf :8080/health" --every=10s
  block-sleep hold /tmp/my-trigger         Block until: echo > /tmp/my-trigger
  block-sleep forever                      Block until: block-sleep stop
  block-sleep status                       Show all active blocks
  block-sleep extend 1h                    Reset a timer block
  block-sleep extend 1h --id=2             Reset a specific block
  block-sleep stop                         Stop all blocks
  block-sleep stop 1                       Stop block #1
`)
}

func parseDuration(s string) time.Duration {
	d, err := parseDurationSafe(s)
	if err != nil {
		fatalf("Invalid duration: %s", s)
	}
	return d
}

func parseDurationSafe(s string) (time.Duration, error) {
	if h, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(h * float64(time.Hour)), nil
	}
	return time.ParseDuration(s)
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
