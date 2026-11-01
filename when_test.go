package main

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/godbus/dbus/v5"
)

type fakeInhibitor struct{ released bool }

func (f *fakeInhibitor) Release()              { f.released = true }
func (f *fakeInhibitor) Lost() <-chan struct{} { return nil }

func newFakeController(grace time.Duration) (*whenController, *[]*fakeInhibitor) {
	var made []*fakeInhibitor
	c := &whenController{
		grace: grace,
		acquire: func() (Inhibitor, error) {
			f := &fakeInhibitor{}
			made = append(made, f)
			return f, nil
		},
	}
	return c, &made
}

func TestControllerArmsAndReleasesWithoutGrace(t *testing.T) {
	c, made := newFakeController(0)
	c.onResult(false)
	if c.armed() || len(*made) != 0 {
		t.Fatal("a failing first check must not arm")
	}
	c.onResult(true)
	c.onResult(true)
	if !c.armed() || len(*made) != 1 {
		t.Fatalf("want one inhibitor held, got armed=%v made=%d", c.armed(), len(*made))
	}
	c.onResult(false)
	if c.armed() || !(*made)[0].released {
		t.Fatal("with no grace a failing check releases at once")
	}
}

func TestControllerGraceKeepsBlockThroughBlip(t *testing.T) {
	c, made := newFakeController(time.Hour)
	c.onResult(true)
	c.onResult(false)
	if !c.armed() || !c.releasing() {
		t.Fatal("within grace the block must still be held")
	}
	c.onResult(false)
	c.onResult(true)
	if !c.armed() || c.releasing() || len(*made) != 1 || (*made)[0].released {
		t.Fatal("passing again within grace keeps the same inhibitor and cancels the release")
	}
}

func TestControllerGraceExpiry(t *testing.T) {
	c, made := newFakeController(10 * time.Millisecond)
	c.onResult(true)
	c.onResult(false)
	select {
	case <-c.graceC:
		c.onGraceExpired()
	case <-time.After(time.Second):
		t.Fatal("grace timer never fired")
	}
	if c.armed() || !(*made)[0].released {
		t.Fatal("expired grace releases the block")
	}
	c.onResult(true)
	if !c.armed() || len(*made) != 2 {
		t.Fatal("re-arms with a fresh inhibitor")
	}
}

func TestControllerAcquireError(t *testing.T) {
	c := &whenController{acquire: func() (Inhibitor, error) { return nil, errors.New("denied") }}
	if err := c.onResult(true); err == nil {
		t.Fatal("acquire errors must surface")
	}
}

func TestRunCheckExitCodes(t *testing.T) {
	if r := runCheck("true", time.Second); !r.ok() {
		t.Fatalf("true: %+v", r)
	}
	if r := runCheck("exit 3", time.Second); r.ok() || r.exitCode != 3 {
		t.Fatalf("exit 3: %+v", r)
	}
}

func TestRunCheckTimeoutKillsProcessGroup(t *testing.T) {
	start := time.Now()
	r := runCheck("sleep 5 & sleep 5; wait", 200*time.Millisecond)
	if r.ok() || r.err == nil {
		t.Fatalf("a timed-out check must fail with an error: %+v", r)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout did not kill the whole group")
	}
}

func TestIsTrigger(t *testing.T) {
	cases := []struct {
		sig  *dbus.Signal
		want bool
	}{
		{&dbus.Signal{Name: prepareForSleep, Body: []any{true}}, false},
		{&dbus.Signal{Name: prepareForSleep, Body: []any{false}}, true},
		{&dbus.Signal{Name: "org.freedesktop.DBus.NameAcquired"}, false},
		{&dbus.Signal{Name: "org.freedesktop.DBus.Properties.PropertiesChanged"}, true},
	}
	for _, c := range cases {
		if got := isTrigger(c.sig); got != c.want {
			t.Errorf("%s %v: got %v", c.sig.Name, c.sig.Body, got)
		}
	}
}

func TestSystemdQuote(t *testing.T) {
	got := systemdQuote(`/home/x y/bin/b$s%"`)
	want := `"/home/x y/bin/b$$s%%\""`
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestParseWhenArgs(t *testing.T) {
	spec, persist, unpersist := parseWhenArgs([]string{
		"test -e /tmp/x", "--on-dbus=type='signal',member='A'", "--on-dbus=type='signal',member='B'",
		"--grace=5s",
	})
	if persist || unpersist || spec.Check != "test -e /tmp/x" || len(spec.OnDBus) != 2 || spec.Grace != "5s" {
		t.Fatalf("%+v", spec)
	}
	if spec.OnDBus[0] != "type='signal',member='A'" {
		t.Fatalf("rule must keep everything after the first '=': %q", spec.OnDBus[0])
	}
}

func TestSpecValidate(t *testing.T) {
	if err := (&WhenSpec{}).validate(); err == nil {
		t.Error("empty check must be rejected")
	}
	if err := (&WhenSpec{Check: "true", Grace: "soon"}).validate(); err == nil {
		t.Error("bad grace must be rejected")
	}
	if err := (&WhenSpec{Check: "true", Timeout: "0s"}).validate(); err == nil {
		t.Error("zero timeout must be rejected")
	}
	tm, err := (&WhenSpec{Check: "true"}).timings()
	if err != nil || tm.grace != defaultWhenGrace || tm.timeout != defaultWhenTimeout || tm.every != 0 {
		t.Errorf("defaults: %+v %v", tm, err)
	}
}

func TestControllerResumeHoldOutlastsGrace(t *testing.T) {
	c, _ := newFakeController(0)
	c.onResult(true)
	c.holdUntil = time.Now().Add(time.Hour)
	c.onResult(false)
	if !c.armed() || !c.releasing() {
		t.Fatal("a failing check right after resume must not release at once")
	}
}

func TestEnvQuote(t *testing.T) {
	if got, want := envQuote(`PATH=/a%b:$HOME`), `"PATH=/a%%b:$HOME"`; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestWhyIsValidUTF8(t *testing.T) {
	why := (&WhenSpec{Check: "true #" + strings.Repeat("a", 50) + "ææææææææ"}).why()
	if !utf8.ValidString(why) {
		t.Fatalf("not valid UTF-8: %q", why)
	}
}

func TestResumeExtendsRunningGrace(t *testing.T) {
	c, _ := newFakeController(10 * time.Millisecond)
	c.onResult(true)
	c.onResult(false)
	c.onResume(time.Now())
	select {
	case <-c.graceC:
		t.Fatal("resume must extend the running grace timer")
	case <-time.After(50 * time.Millisecond):
	}
	if !c.armed() {
		t.Fatal("still armed after resume")
	}
}
