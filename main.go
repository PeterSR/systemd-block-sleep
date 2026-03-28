package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const defaultDuration = 3 * time.Hour

func main() {
	args := os.Args[1:]

	if len(args) == 0 {
		start(defaultDuration)
		return
	}

	switch args[0] {
	case "_daemon":
		if len(args) < 2 {
			fatalf("usage: block-sleep _daemon <end-time>")
		}
		runDaemon(args[1])
	case "start":
		d := defaultDuration
		if len(args) > 1 {
			d = parseDuration(args[1])
		}
		start(d)
	case "status", "remaining":
		showStatus()
	case "extend":
		if len(args) < 2 {
			fatalf("Usage: block-sleep extend <duration>")
		}
		extend(parseDuration(args[1]))
	case "list-all":
		listAll()
	case "stop":
		stop()
	case "install-sudoers":
		installSudoers()
	case "-h", "--help", "help":
		usage()
	default:
		d, err := parseDurationSafe(args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", args[0])
			usage()
			os.Exit(1)
		}
		start(d)
	}
}

func usage() {
	fmt.Print(`Usage: block-sleep [command] [duration]

Block system sleep using systemd-inhibit.

Commands:
  [duration]       Block sleep for the given duration (default: 3h)
  status           Show remaining time
  extend <dur>     Reset the block to <dur> from now
  list-all         List all sleep inhibitors on the system
  stop             Stop blocking sleep
  install-sudoers  Install sudoers file for passwordless operation
  help             Show this help

Duration formats:
  2            2 hours
  1.5          1 hour 30 minutes
  2h30m        2 hours 30 minutes
  45m          45 minutes

Examples:
  block-sleep              Block for 3 hours
  block-sleep 2            Block for 2 hours
  block-sleep 1h30m        Block for 1 hour 30 minutes
  block-sleep status       Show remaining time
  block-sleep extend 1h    Reset to 1 hour from now
  block-sleep stop         Stop blocking
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
