package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	defaultWhenGrace   = 30 * time.Second
	defaultWhenTimeout = 10 * time.Second
	persistUnitName    = "block-sleep-when.service"
	// After waking up, the network is often still reconnecting; a short grace would release the
	// block before it is back.
	resumeGrace         = 2 * time.Minute
	prepareForSleep     = "org.freedesktop.login1.Manager.PrepareForSleep"
	prepareForSleepRule = "type='signal',sender='org.freedesktop.login1',interface='org.freedesktop.login1.Manager',member='PrepareForSleep'"
)

// WhenSpec is a conditional block: hold the inhibitor while Check exits 0, re-running Check only when
// a trigger fires.
type WhenSpec struct {
	Check         string   `json:"check" toml:"check"`
	OnDBus        []string `json:"on_dbus,omitempty" toml:"on_dbus"`
	OnDBusSession []string `json:"on_dbus_session,omitempty" toml:"on_dbus_session"`
	Every         string   `json:"every,omitempty" toml:"every"`
	Grace         string   `json:"grace,omitempty" toml:"grace"`
	Timeout       string   `json:"timeout,omitempty" toml:"timeout"`
	What          string   `json:"what,omitempty" toml:"what"`
}

type whenTimings struct {
	every, grace, timeout time.Duration
}

func (s *WhenSpec) timings() (whenTimings, error) {
	t := whenTimings{grace: defaultWhenGrace, timeout: defaultWhenTimeout}
	parse := func(name, v string, dst *time.Duration) error {
		if v == "" {
			return nil
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return fmt.Errorf("invalid %s: %q", name, v)
		}
		*dst = d
		return nil
	}
	if err := parse("every", s.Every, &t.every); err != nil {
		return t, err
	}
	if err := parse("grace", s.Grace, &t.grace); err != nil {
		return t, err
	}
	if err := parse("timeout", s.Timeout, &t.timeout); err != nil {
		return t, err
	}
	if t.timeout == 0 {
		return t, fmt.Errorf("timeout must be above zero")
	}
	return t, nil
}

func (s *WhenSpec) validate() error {
	if strings.TrimSpace(s.Check) == "" {
		return fmt.Errorf("no condition command given")
	}
	_, err := s.timings()
	return err
}

func (s *WhenSpec) why() string {
	// logind rejects strings that are not valid UTF-8, so cut on a rune boundary.
	r := []rune(strings.ToValidUTF8(s.Check, "?"))
	if len(r) > 60 {
		r = append(r[:57], []rune("...")...)
	}
	return "block-sleep when " + string(r)
}

// ---- CLI ----

func parseWhenArgs(args []string) (spec WhenSpec, persist, unpersist bool) {
	var positional []string
	for _, a := range args {
		k, v, hasValue := strings.Cut(a, "=")
		switch {
		case a == "--persist":
			persist = true
		case a == "--unpersist":
			unpersist = true
		case k == "--on-dbus" && hasValue:
			spec.OnDBus = append(spec.OnDBus, v)
		case k == "--on-dbus-session" && hasValue:
			spec.OnDBusSession = append(spec.OnDBusSession, v)
		case k == "--every" && hasValue:
			spec.Every = v
		case k == "--grace" && hasValue:
			spec.Grace = v
		case k == "--timeout" && hasValue:
			spec.Timeout = v
		case strings.HasPrefix(a, "--"):
			fatalf("Unknown option for when: %s", a)
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) > 1 {
		fatalf("Usage: block-sleep when '<command>' [--on-dbus=<rule>]... (quote the command as one argument)")
	}
	if len(positional) == 1 {
		spec.Check = positional[0]
	}
	return
}

func cmdWhen(args []string, what, configFile string) {
	spec, persist, unpersist := parseWhenArgs(args)
	if unpersist {
		unpersistWhen()
		return
	}
	if persist {
		if spec.Check != "" || len(spec.OnDBus)+len(spec.OnDBusSession) > 0 || spec.Every != "" || spec.Grace != "" || spec.Timeout != "" {
			fatalf("--persist reads the rule from the [when] section of %s; put it there instead of on the command line.", configPathOr(configFile))
		}
		persistWhen(what, configFile)
		return
	}
	if spec.Check == "" {
		if len(spec.OnDBus)+len(spec.OnDBusSession) > 0 {
			fatalf("Usage: block-sleep when '<command>' [--on-dbus=<rule>]...")
		}
		cfgSpec := loadWhenConfig(configFile)
		for _, o := range []struct{ flag, cfg *string }{
			{&spec.Every, &cfgSpec.Every}, {&spec.Grace, &cfgSpec.Grace}, {&spec.Timeout, &cfgSpec.Timeout},
		} {
			if *o.flag != "" {
				*o.cfg = *o.flag
			}
		}
		spec = *cfgSpec
	}
	if what != "" {
		spec.What = what
	}
	if spec.What == "" {
		spec.What = defaultWhat
	}
	if err := spec.validate(); err != nil {
		fatalf("%v", err)
	}
	if err := checkMatchRules(&spec); err != nil {
		fatalf("%v", err)
	}
	startWhenTransient(&spec)
}

func loadWhenConfig(configFile string) *WhenSpec {
	cfg, err := loadConfig(configFile)
	if err != nil {
		fatalf("Failed to load config: %v", err)
	}
	if cfg == nil || cfg.When.Check == "" {
		fatalf("No condition given and no [when] section with 'check' in %s.\n\nExample:\n\n  [when]\n  check = \"test -e /run/some-flag\"\n  on_dbus = [\"type='signal',sender='org.freedesktop.UPower',member='PropertiesChanged'\"]", configPathOr(configFile))
	}
	spec := cfg.When
	return &spec
}

func configPathOr(configFile string) string {
	if configFile != "" {
		return configFile
	}
	return configPath()
}

// checkMatchRules registers each rule once so a typo fails here instead of silently never firing.
func checkMatchRules(spec *WhenSpec) error {
	if len(spec.OnDBus) > 0 {
		conn, err := dbus.ConnectSystemBus()
		if err != nil {
			return fmt.Errorf("system bus: %w", err)
		}
		defer conn.Close()
		if err := addMatches(conn, spec.OnDBus); err != nil {
			return err
		}
	}
	if len(spec.OnDBusSession) > 0 {
		conn, err := dbus.ConnectSessionBus()
		if err != nil {
			return fmt.Errorf("session bus: %w", err)
		}
		defer conn.Close()
		if err := addMatches(conn, spec.OnDBusSession); err != nil {
			return err
		}
	}
	return nil
}

func addMatches(conn *dbus.Conn, rules []string) error {
	for _, r := range rules {
		if err := conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, r).Err; err != nil {
			return fmt.Errorf("invalid D-Bus match rule %q: %v", r, err)
		}
	}
	return nil
}

// startWhenTransient runs the watcher as a transient user unit, so it gets restart-on-failure and
// journal logging. The spec travels base64-encoded so systemd's $ and % expansion cannot touch it.
func startWhenTransient(spec *WhenSpec) {
	if err := ensureStateDir(); err != nil {
		fatalf("Failed to create state directory: %v", err)
	}
	id, err := nextID()
	if err != nil {
		fatalf("Failed to allocate block ID: %v", err)
	}
	unit := fmt.Sprintf("block-sleep-when-%d.service", id)
	state := &State{ID: id, What: spec.What, Mode: "when", When: spec, Unit: unit, Started: time.Now()}
	if err := writeState(state); err != nil {
		fatalf("Failed to write state: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		removeState(id)
		fatalf("Failed to find executable: %v", err)
	}
	raw, _ := json.Marshal(spec)
	encoded := base64.RawURLEncoding.EncodeToString(raw)

	out, err := exec.Command("systemd-run", "--user", "--quiet", "--collect",
		"--unit="+unit,
		"--description=block-sleep when (#"+strconv.Itoa(id)+")",
		"--setenv=PATH",
		"--property=Restart=on-failure", "--property=RestartSec=5",
		"--property=StartLimitIntervalSec=120", "--property=StartLimitBurst=5",
		exe, "_when", "--id="+strconv.Itoa(id), "--spec="+encoded).CombinedOutput()
	if err != nil {
		removeState(id)
		fatalf("systemd-run failed: %v\n%s", err, out)
	}

	timings, _ := spec.timings()
	if s := waitForFirstCheck(id, timings.timeout+5*time.Second); s != nil {
		now := "condition false right now, waiting"
		if s.Armed {
			now = "blocking now"
		}
		fmt.Printf("Sleep blocked while %q succeeds (%s) [#%d]\nUnit: %s (logs: journalctl --user -u %s)\n", spec.Check, now, id, unit, unit)
		return
	}
	exec.Command("systemctl", "--user", "stop", unit).Run()
	removeState(id)
	fatalf("The watcher did not start. See: journalctl --user -u %s", unit)
}

// waitForFirstCheck returns the state once the watcher has run its first check and, if it passed,
// taken the block, so a refused inhibitor is reported here instead of after "Sleep blocked".
func waitForFirstCheck(id int, limit time.Duration) *State {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if s, err := readState(id); err == nil && s.PID > 0 && s.LastExit != nil {
			return s
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

func userUnitDir() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fatalf("Cannot find home directory: %v", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user")
}

// systemdQuote makes a single ExecStart word literal: % and $ are expanded by systemd otherwise.
func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	s = strings.ReplaceAll(s, "$", "$$")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func persistWhen(what, configFile string) {
	spec := loadWhenConfig(configFile)
	if what != "" {
		spec.What = what
	}
	if err := spec.validate(); err != nil {
		fatalf("[when] in %s: %v", configPathOr(configFile), err)
	}
	if err := checkMatchRules(spec); err != nil {
		fatalf("[when] in %s: %v", configPathOr(configFile), err)
	}
	exe, err := os.Executable()
	if err != nil {
		fatalf("Failed to find executable: %v", err)
	}

	// Pinned, since the user manager's environment may resolve the default config path differently.
	abs, err := filepath.Abs(configPathOr(configFile))
	if err != nil {
		fatalf("Invalid config path: %v", err)
	}
	execStart := systemdQuote(exe) + " _when --from-config " + systemdQuote("--config="+abs)
	if what != "" {
		execStart += " " + systemdQuote("--what="+what)
	}
	unit := fmt.Sprintf(`[Unit]
Description=block-sleep when (rule from the [when] section of the block-sleep config)
StartLimitIntervalSec=120
StartLimitBurst=5

[Service]
ExecStart=%s
Environment=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, execStart, envQuote("PATH="+os.Getenv("PATH")))

	dir := userUnitDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		fatalf("Failed to create %s: %v", dir, err)
	}
	path := filepath.Join(dir, persistUnitName)
	if err := os.WriteFile(path, []byte(unit), 0644); err != nil {
		fatalf("Failed to write %s: %v", path, err)
	}
	restartedAt := time.Now()
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", persistUnitName},
		{"--user", "restart", persistUnitName},
	} {
		if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
			fatalf("systemctl %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	timings, _ := spec.timings()
	st := waitForPersistentCheck(restartedAt, timings.timeout+5*time.Second)
	if st == nil {
		fatalf("Installed %s, but the watcher has not run its first check. See: journalctl --user -u %s", path, persistUnitName)
	}
	now := "condition false right now, waiting"
	if st.Armed {
		now = "blocking now"
	}
	fmt.Printf("Installed and started %s (%s)\nRule: %q\nAfter editing the [when] config: systemctl --user restart %s\n", path, now, spec.Check, persistUnitName)
	if onPath, err := exec.LookPath("block-sleep"); err == nil {
		if resolved, err := filepath.EvalSymlinks(onPath); err == nil && resolved != exe {
			fmt.Printf("Note: the unit runs %s, not the block-sleep on your PATH (%s). Re-run --persist from the one you want upgrades to reach.\n", exe, resolved)
		}
	}
	if out, err := exec.Command("loginctl", "show-user", strconv.Itoa(os.Getuid()), "--property=Linger", "--value").Output(); err == nil && strings.TrimSpace(string(out)) != "yes" {
		fmt.Printf("Note: without lingering, the watcher stops when you log out. To keep it: loginctl enable-linger\n")
	}
}

func unpersistWhen() {
	path := filepath.Join(userUnitDir(), persistUnitName)
	exec.Command("systemctl", "--user", "disable", "--now", persistUnitName).Run()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fatalf("Failed to remove %s: %v", path, err)
	}
	exec.Command("systemctl", "--user", "daemon-reload").Run()
	fmt.Printf("Removed %s\n", persistUnitName)
}

// ---- watcher (runs inside a user unit) ----

func runWhen(args []string, what, configFile string) {
	var id = -1
	var specArg string
	fromConfig := false
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--id="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--id="))
			if err != nil {
				fatalf("invalid --id")
			}
			id = n
		case strings.HasPrefix(a, "--spec="):
			specArg = strings.TrimPrefix(a, "--spec=")
		case a == "--from-config":
			fromConfig = true
		}
	}

	var spec *WhenSpec
	var unit string
	if fromConfig {
		spec = loadWhenConfig(configFile)
		unit = persistUnitName
	} else {
		raw, err := base64.RawURLEncoding.DecodeString(specArg)
		if err != nil {
			fatalf("invalid --spec: %v", err)
		}
		spec = &WhenSpec{}
		if err := json.Unmarshal(raw, spec); err != nil {
			fatalf("invalid --spec: %v", err)
		}
		if id >= 0 {
			unit = fmt.Sprintf("block-sleep-when-%d.service", id)
		}
	}
	if what != "" {
		spec.What = what
	}
	if spec.What == "" {
		spec.What = defaultWhat
	}
	if err := spec.validate(); err != nil {
		fatalf("%v", err)
	}
	timings, _ := spec.timings()

	if err := ensureStateDir(); err != nil {
		fatalf("Failed to create state directory: %v", err)
	}
	if id < 0 {
		if prev := persistentState(); prev != nil {
			id = prev.ID
		}
	}
	if id < 0 {
		n, err := nextID()
		if err != nil {
			fatalf("Failed to allocate block ID: %v", err)
		}
		id = n
	}

	sysConn, err := dbus.ConnectSystemBus()
	if err != nil {
		fatalf("system bus: %v", err)
	}
	// Subscribe before the first check, so no change between the two can be missed.
	if err := addMatches(sysConn, append([]string{prepareForSleepRule}, spec.OnDBus...)); err != nil {
		fatalf("%v", err)
	}
	sysSig := make(chan *dbus.Signal, 32)
	sysConn.Signal(sysSig)

	var sessSig chan *dbus.Signal
	if len(spec.OnDBusSession) > 0 {
		sessConn, err := dbus.ConnectSessionBus()
		if err != nil {
			fatalf("session bus: %v", err)
		}
		if err := addMatches(sessConn, spec.OnDBusSession); err != nil {
			fatalf("%v", err)
		}
		sessSig = make(chan *dbus.Signal, 32)
		sessConn.Signal(sessSig)
	}

	osSig := make(chan os.Signal, 4)
	signal.Notify(osSig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)

	state := &State{ID: id, PID: os.Getpid(), What: spec.What, Mode: "when", When: spec, Unit: unit, Started: time.Now()}
	ctl := &whenController{
		grace: timings.grace,
		acquire: func() (Inhibitor, error) {
			return acquireInhibitor(sysConn, spec.What, spec.why())
		},
	}
	save := func() {
		state.Armed = ctl.armed()
		state.Releasing = ctl.releasing()
		if err := writeState(state); err != nil {
			fmt.Fprintf(os.Stderr, "write state: %v\n", err)
		}
	}
	// A failure exit keeps the state file: the unit restarts under the same ID, and while it is
	// active or restarting, isBlockActive keeps others from reusing the ID.
	exit := func(code int) {
		ctl.stop()
		if code == 0 {
			removeState(id)
		}
		os.Exit(code)
	}
	save()

	trig := make(chan struct{}, 1)
	poke := func() {
		select {
		case trig <- struct{}{}:
		default:
		}
	}
	results := make(chan checkResult)
	go func() {
		for range trig {
			results <- runCheck(spec.Check, timings.timeout)
		}
	}()
	poke()

	var tick <-chan time.Time
	if timings.every > 0 {
		t := time.NewTicker(timings.every)
		defer t.Stop()
		tick = t.C
	}

	for {
		select {
		case r := <-results:
			state.LastCheck = time.Now()
			code := r.exitCode
			state.LastExit = &code
			if r.err != nil {
				fmt.Fprintf(os.Stderr, "check: %v\n", r.err)
			}
			if err := ctl.onResult(r.ok()); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				exit(1)
			}
			save()
		case <-ctl.graceC:
			ctl.onGraceExpired()
			save()
		case <-ctl.lost():
			fmt.Fprintln(os.Stderr, "the inhibitor ended unexpectedly")
			exit(1)
		case s, ok := <-sysSig:
			if !ok {
				fmt.Fprintln(os.Stderr, "system bus connection closed")
				exit(1)
			}
			if isResume(s) {
				ctl.onResume(time.Now())
			}
			if isTrigger(s) {
				poke()
			}
		case s, ok := <-sessSig:
			if !ok {
				fmt.Fprintln(os.Stderr, "session bus connection closed")
				exit(1)
			}
			if isTrigger(s) {
				poke()
			}
		case <-tick:
			poke()
		case sig := <-osSig:
			if sig == syscall.SIGUSR1 {
				poke()
				continue
			}
			exit(0)
		}
	}
}

// isTrigger drops bus housekeeping signals and the going-to-sleep half of PrepareForSleep; waking up
// re-checks, since the world may have changed while suspended.
func isTrigger(s *dbus.Signal) bool {
	switch s.Name {
	case "org.freedesktop.DBus.NameAcquired", "org.freedesktop.DBus.NameLost":
		return false
	case prepareForSleep:
		if len(s.Body) > 0 {
			if goingDown, ok := s.Body[0].(bool); ok && goingDown {
				return false
			}
		}
	}
	return true
}

type checkResult struct {
	exitCode int
	err      error
}

func (r checkResult) ok() bool { return r.err == nil && r.exitCode == 0 }

// runCheck runs the condition in its own process group so a timeout kills everything it started.
func runCheck(command string, timeout time.Duration) checkResult {
	cmd := exec.Command("sh", "-c", command)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return checkResult{exitCode: -1, err: err}
	}
	var timedOut atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		timedOut.Store(true)
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	})
	err := cmd.Wait()
	timer.Stop()
	if timedOut.Load() {
		return checkResult{exitCode: -1, err: fmt.Errorf("timed out after %s", timeout)}
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return checkResult{exitCode: ee.ExitCode()}
		}
		return checkResult{exitCode: -1, err: err}
	}
	return checkResult{exitCode: 0}
}

// whenController holds the inhibitor while checks pass, and keeps it for a grace period after a
// failing check so a brief blip (Wi-Fi reconnect, dock replug) does not release it.
type whenController struct {
	grace     time.Duration
	holdUntil time.Time
	acquire   func() (Inhibitor, error)

	inh        Inhibitor
	graceTimer *time.Timer
	graceC     <-chan time.Time
}

func (c *whenController) armed() bool     { return c.inh != nil }
func (c *whenController) releasing() bool { return c.graceTimer != nil }

func (c *whenController) lost() <-chan struct{} {
	if c.inh == nil {
		return nil
	}
	return c.inh.Lost()
}

func (c *whenController) onResult(ok bool) error {
	if ok {
		c.cancelGrace()
		if c.inh == nil {
			inh, err := c.acquire()
			if err != nil {
				return err
			}
			c.inh = inh
		}
		return nil
	}
	if c.inh == nil || c.graceTimer != nil {
		return nil
	}
	grace := max(c.grace, time.Until(c.holdUntil))
	if grace <= 0 {
		c.release()
		return nil
	}
	c.graceTimer = time.NewTimer(grace)
	c.graceC = c.graceTimer.C
	return nil
}

// onResume extends a grace period that is already running too: a check that failed just before
// suspend (Wi-Fi going down) would otherwise release the block shortly after waking.
func (c *whenController) onResume(now time.Time) {
	c.holdUntil = now.Add(resumeGrace)
	if c.graceTimer != nil {
		c.graceTimer.Stop()
		c.graceTimer = time.NewTimer(max(c.grace, time.Until(c.holdUntil)))
		c.graceC = c.graceTimer.C
	}
}

func (c *whenController) onGraceExpired() {
	c.graceTimer = nil
	c.graceC = nil
	c.release()
}

func (c *whenController) cancelGrace() {
	if c.graceTimer != nil {
		c.graceTimer.Stop()
		c.graceTimer = nil
		c.graceC = nil
	}
}

func (c *whenController) release() {
	if c.inh != nil {
		c.inh.Release()
		c.inh = nil
	}
}

func (c *whenController) stop() {
	c.cancelGrace()
	c.release()
}

func describeWhen(s *State) string {
	status := "waiting"
	switch {
	case s.Unit != "" && !unitRunning(s):
		status = "restarting"
	case s.Armed && s.Releasing:
		status = "blocking, releasing after grace"
	case s.Armed:
		status = "blocking"
	}
	check := ""
	if s.When != nil {
		check = s.When.Check
	}
	last := ""
	if !s.LastCheck.IsZero() && s.LastExit != nil {
		last = fmt.Sprintf(", last check %s ago exit %d", formatDuration(time.Since(s.LastCheck)), *s.LastExit)
	}
	kind := "when"
	if s.Unit == persistUnitName {
		kind = "when (persistent)"
	}
	return fmt.Sprintf("%s %q (%s%s)", kind, check, status, last)
}

func stopUnit(s *State) {
	if out, err := exec.Command("systemctl", "--user", "stop", s.Unit).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "systemctl --user stop %s: %v\n%s", s.Unit, err, out)
	}
}

func recheck(id int) {
	states, err := activeStates()
	if err != nil {
		fatalf("Failed to read state: %v", err)
	}
	n, starting := 0, 0
	for _, s := range states {
		if s.Mode != "when" || (id >= 0 && s.ID != id) {
			continue
		}
		// While a unit is starting or restarting the PID is 0 or stale, and it re-checks at start anyway.
		if s.PID <= 0 || (s.Unit != "" && !unitRunning(s)) {
			starting++
			continue
		}
		syscall.Kill(s.PID, syscall.SIGUSR1)
		n++
	}
	if n == 0 && starting > 0 {
		fmt.Println("The 'when' block is (re)starting; it checks its condition at start.")
		return
	}
	if n == 0 {
		fatalf("No matching 'when' block.")
	}
}

func isResume(s *dbus.Signal) bool {
	if s.Name != prepareForSleep || len(s.Body) == 0 {
		return false
	}
	goingDown, ok := s.Body[0].(bool)
	return ok && !goingDown
}

// envQuote makes an Environment= assignment literal; systemd expands % specifiers there.
func envQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func persistentState() *State {
	all, _ := allStates()
	for _, s := range all {
		if s.Unit == persistUnitName {
			return s
		}
	}
	return nil
}

// unitAlive treats a unit-backed block as alive while its unit is running or restarting. A state
// written moments ago counts too: the CLI writes it before systemd-run has started the unit.
func unitAlive(s *State) bool {
	if s.PID == 0 && time.Since(s.Started) < 10*time.Second {
		return true
	}
	switch unitState(s.Unit) {
	case "active", "activating", "reloading", "deactivating":
		return true
	case "":
		// systemctl could not reach the user manager: fall back to the PID rather than delete a live block.
		return s.PID > 0 && processExists(s.PID)
	}
	return false
}

// unitRunning is true while the watcher is up; with no answer from systemctl it trusts the PID.
func unitRunning(s *State) bool {
	switch unitState(s.Unit) {
	case "active":
		return true
	case "":
		return s.PID > 0 && processExists(s.PID)
	}
	return false
}

func unitState(unit string) string {
	out, _ := exec.Command("systemctl", "--user", "is-active", unit).Output()
	return strings.TrimSpace(string(out))
}

// waitForPersistentCheck ignores state left by an earlier run of the unit.
func waitForPersistentCheck(since time.Time, limit time.Duration) *State {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if s := persistentState(); s != nil && !s.Started.Before(since) && s.PID > 0 && s.LastExit != nil {
			return s
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}
