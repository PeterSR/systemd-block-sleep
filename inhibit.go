package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/godbus/dbus/v5"
)

// Inhibitor is a held sleep block. Release ends it. Lost is closed if the block ends on its own
// (nil when that cannot happen).
type Inhibitor interface {
	Release()
	Lost() <-chan struct{}
}

type fdInhibitor struct{ f *os.File }

func (i *fdInhibitor) Release()              { i.f.Close() }
func (i *fdInhibitor) Lost() <-chan struct{} { return nil }

type sudoInhibitor struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{}
}

// Closing stdin ends the `cat` that systemd-inhibit wraps, which releases the block.
func (i *sudoInhibitor) Release() {
	i.stdin.Close()
	<-i.done
}

func (i *sudoInhibitor) Lost() <-chan struct{} { return i.done }

// acquireInhibitor asks logind directly over D-Bus. Without root this is allowed for processes in a
// user session or (systemd 256+) the user manager; where polkit refuses, it falls back to the
// passwordless sudo rule that `install-sudoers` sets up.
func acquireInhibitor(conn *dbus.Conn, what, why string) (Inhibitor, error) {
	var fd dbus.UnixFD
	obj := conn.Object("org.freedesktop.login1", "/org/freedesktop/login1")
	err := obj.Call("org.freedesktop.login1.Manager.Inhibit", 0, what, "block-sleep", why, "block").Store(&fd)
	if err == nil {
		return &fdInhibitor{f: os.NewFile(uintptr(fd), "inhibit")}, nil
	}
	if !isAuthError(err) {
		return nil, fmt.Errorf("logind Inhibit: %w", err)
	}
	inh, sudoErr := sudoInhibit(what, why)
	if sudoErr != nil {
		return nil, fmt.Errorf("logind refused (%v) and the sudo fallback failed (%v); try: block-sleep install-sudoers", err, sudoErr)
	}
	return inh, nil
}

func isAuthError(err error) bool {
	var de dbus.Error
	if !errors.As(err, &de) {
		return false
	}
	switch de.Name {
	case "org.freedesktop.DBus.Error.AccessDenied",
		"org.freedesktop.DBus.Error.InteractiveAuthorizationRequired":
		return true
	}
	return false
}

func sudoInhibit(what, why string) (Inhibitor, error) {
	args := []string{"systemd-inhibit", "--what=" + what, "--why=" + why, "--mode=block", "cat"}
	// `sudo -l <cmd>` asks whether the rule allows it, so a missing rule fails here rather than as a
	// process that starts and dies at once.
	if out, err := exec.Command("sudo", append([]string{"-n", "-l"}, args...)...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("sudo does not allow systemd-inhibit without a password: %v %s", err, out)
	}
	cmd := exec.Command("sudo", append([]string{"-n"}, args...)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	inh := &sudoInhibitor{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	go func() {
		cmd.Wait()
		close(inh.done)
	}()
	// `sudo -l` passes when any rule matches, even one that wants a password, so also catch a
	// systemd-inhibit that exits at once.
	select {
	case <-inh.done:
		return nil, fmt.Errorf("sudo systemd-inhibit exited at once (a NOPASSWD rule is needed): %v", cmd.ProcessState)
	case <-time.After(time.Second):
	}
	return inh, nil
}
