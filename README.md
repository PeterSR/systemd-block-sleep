# block-sleep

Temporarily block system sleep on Linux using `systemd-inhibit`. Supports multiple blocking modes: duration, deadline, process watching, command polling, pipe signaling, and indefinite.

## Install

Requires Go 1.22+ and a Linux system with `systemd-inhibit`.

```sh
make install   # builds and copies to ~/bin/
```

### Passwordless sudo (recommended)

`block-sleep` needs sudo to call `systemd-inhibit`. To avoid password prompts, install a sudoers rule:

```sh
block-sleep install-sudoers
```

This is idempotent and writes `/etc/sudoers.d/block-sleep-<username>` with a rule scoped to `systemd-inhibit`:

```
<username> ALL=(root) NOPASSWD: /usr/bin/systemd-inhibit --what=* --why=* --mode=block *
```

## Modes

### `for` — Block for a duration

```sh
block-sleep for 2h           # 2 hours
block-sleep for 1.5          # 1 hour 30 minutes (plain numbers = hours)
block-sleep for 45m          # 45 minutes
block-sleep for 2h30m        # 2 hours 30 minutes
```

Running bare `block-sleep` with no arguments uses the default action from the config file (see below). If no config file exists, it shows an error directing you to create one or specify a command.


### `until` — Block until a time

```sh
block-sleep until 22:00                  # today, or tomorrow if already past
block-sleep until 2026-03-30T08:00       # absolute datetime
block-sleep until 2026-03-30 08:00       # same, with space
```

### `run` — Block while a command runs

Runs a command in the foreground with full stdio passthrough. Sleep is blocked while the command runs, and the exit code is forwarded.

```sh
block-sleep run -- make build
block-sleep run -- rsync -av src/ dest/
block-sleep run -- docker compose up
```

### `await` — Block while a PID or condition is true

Watch a process by PID (uses `pidfd_open` for zero-polling, event-driven exit detection on Linux 5.3+):

```sh
rsync -av src/ dest/ &
block-sleep await $!           # block while rsync is alive
```

Poll a shell command (block while it exits 0):

```sh
block-sleep await "ss -t state established | grep -q :22" --every=30s
block-sleep await "pgrep -x firefox"
block-sleep await "curl -sf http://localhost:8080/health" --every=10s
```

Default poll interval is 30 seconds.

### `hold` — Block until a pipe receives data

Creates a FIFO and blocks until something writes to it. Useful for script-based control.

```sh
block-sleep hold /tmp/my-trigger

# Later, from another terminal or script:
echo > /tmp/my-trigger       # releases the block
```

### `forever` — Block indefinitely

Blocks until explicitly stopped with `block-sleep stop`.

```sh
block-sleep forever
# ...
block-sleep stop
```

### `when`: Block whenever a condition holds

Unlike the other modes, `when` does not end. It holds the block while a command of yours exits 0, releases it when the command fails, and blocks again when it succeeds again. The command is not polled: it is re-run only when something you name happens.

```sh
block-sleep when '<command>' [--on-dbus=<match rule>]... [--on-dbus-session=<match rule>]...
                            [--every=<dur>] [--grace=<dur>] [--timeout=<dur>]
```

| Trigger | Re-runs the command when |
|---------|--------------------------|
| (always) | the watcher starts, and the machine wakes from suspend |
| `--on-dbus=<rule>` | a system bus signal matches the [D-Bus match rule](https://dbus.freedesktop.org/doc/dbus-specification.html#message-bus-routing-match-rules) (repeatable) |
| `--on-dbus-session=<rule>` | the same, on your session bus |
| `block-sleep recheck [id]` | you or a script says so |
| `--every=<dur>` | the interval passes (off unless given; for conditions nothing announces) |

- `--grace` (default `30s`): after the command fails, keep blocking this long, so a brief blip such as a Wi-Fi reconnect does not release the block. Blocking again is immediate. After a wake from suspend the block is held until at least two minutes after the wake, since the network is often still reconnecting.
- `--timeout` (default `10s`): a check that runs longer is killed, with its whole process group, and counts as failed.
- A trigger arriving while a check runs queues one more run; a burst of triggers never queues more than that.
- Match rules are checked at start, so a typo fails at once instead of never firing.

The watcher runs as a transient user service (`block-sleep-when-<id>.service`), so the command's output and any errors are in `journalctl --user -u block-sleep-when-<id>`. The command runs with your systemd user manager's environment, plus the `PATH` of the shell that started it (for `--persist`: the shell that ran `--persist`). Other variables from your shell are not passed.

#### Example: on AC power and on a given Wi-Fi network

```sh
block-sleep when '[ "$(busctl get-property org.freedesktop.UPower /org/freedesktop/UPower org.freedesktop.UPower OnBattery)" = "b false" ] && nmcli -t -f ACTIVE,SSID dev wifi list --rescan no | grep -qxF "yes:HomeNetwork"' \
  --on-dbus="type='signal',sender='org.freedesktop.UPower',path='/org/freedesktop/UPower',interface='org.freedesktop.DBus.Properties',member='PropertiesChanged',arg0='org.freedesktop.UPower'" \
  --on-dbus="type='signal',sender='org.freedesktop.NetworkManager',interface='org.freedesktop.NetworkManager.Connection.Active',member='StateChanged'" \
  --on-dbus="type='signal',sender='org.freedesktop.DBus',member='NameOwnerChanged',arg0='org.freedesktop.NetworkManager'"
```

- The UPower rule fires when the machine switches between AC and battery. It does not fire on battery level changes, which UPower announces on other object paths.
- The first NetworkManager rule fires when any connection comes up or goes down, including a switch to another network. Roaming between access points of the same network does not fire it.
- The second NetworkManager rule fires when NetworkManager restarts, which leaves connections up and so sends no state change.
- `--rescan no` keeps `nmcli` from starting a Wi-Fi scan on every check. In `-t` output a `:` inside an SSID is escaped as `\:`.
- UPower is used rather than `/sys/class/power_supply/AC/online` because adapter names vary between machines (`AC`, `ADP1`, `ACAD`, ...).

#### Keep it across logins: `--persist`

Put the rule in the `[when]` section of the config file and install it as a user service that starts at every login:

```toml
[when]
check = '''[ "$(busctl get-property org.freedesktop.UPower /org/freedesktop/UPower org.freedesktop.UPower OnBattery)" = "b false" ] && nmcli -t -f ACTIVE,SSID dev wifi list --rescan no | grep -qxF "yes:HomeNetwork"'''
on_dbus = [
  "type='signal',sender='org.freedesktop.UPower',path='/org/freedesktop/UPower',interface='org.freedesktop.DBus.Properties',member='PropertiesChanged',arg0='org.freedesktop.UPower'",
  "type='signal',sender='org.freedesktop.NetworkManager',interface='org.freedesktop.NetworkManager.Connection.Active',member='StateChanged'",
  "type='signal',sender='org.freedesktop.DBus',member='NameOwnerChanged',arg0='org.freedesktop.NetworkManager'",
]
# on_dbus_session = []
# every = ""
# grace = "30s"
# timeout = "10s"
# what = "sleep"
```

```sh
block-sleep when --persist      # writes ~/.config/systemd/user/block-sleep-when.service, enables and starts it
systemctl --user restart block-sleep-when.service   # after editing [when]
block-sleep when --unpersist    # stops, disables and removes it
```

The unit only points at the config, so the rule never has to survive systemd's quoting. It runs the `block-sleep` binary that ran `--persist`, so run it from the installed copy, not a build in a source tree. `block-sleep when` without a command runs the `[when]` rule once, as a non-persistent block. A bare `block-sleep stop` leaves the persistent block running; `block-sleep stop <id>` stops it until the next login.

Things to know:

- **Lid close.** By default logind suspends on lid close even while a `sleep` block is held (`LidSwitchIgnoreInhibited=yes`). To also keep the machine awake with the lid closed, add `--what=sleep:handle-lid-switch` (or `what = "sleep:handle-lid-switch"` in `[when]`).
- **Logout.** Without lingering, your user manager, and the watcher with it, stops when you log out. `loginctl enable-linger` keeps it running.
- **No sudo needed on systemd 256 and later.** `when` asks logind for the block directly, which polkit allows for your user manager from systemd 256. On older systemd it falls back to the `install-sudoers` rule.
- **Recheck from root hooks.** A NetworkManager dispatcher script or a udev `RUN` runs as root, whose runtime directory is not yours: use `runuser -u <you> -- env XDG_RUNTIME_DIR=/run/user/<uid> block-sleep recheck`.

## Multiple Concurrent Blocks

You can have multiple blocks active simultaneously. Each gets an auto-assigned ID. The system stays awake as long as ANY block is active (this is handled natively by `systemd-inhibit`).

```sh
$ block-sleep for 2h
Sleep blocked for 2h00m (until 15:30) [#1]

$ block-sleep await 1234
Sleep blocked while PID 1234 is alive [#2]

$ block-sleep status
#1  for (1h42m remaining, until 15:30) [sleep]
#2  await pid 1234 (18m elapsed) [sleep]

$ block-sleep stop 1           # stop only block #1
Block #1 stopped.

$ block-sleep stop             # stop all remaining blocks
Sleep block stopped.
```

### Management Commands

```sh
block-sleep status                  # show all active blocks
block-sleep stop                    # stop ALL active blocks (except a persistent `when`)
block-sleep stop <id>               # stop a specific block
block-sleep recheck [id]            # re-run the condition of `when` blocks now
block-sleep extend 1h               # extend a timer block (for/until)
block-sleep extend 1h --id=2        # extend a specific block
block-sleep list-all                # list all systemd inhibitors
```

`extend` only works on timer-based blocks (`for` and `until`). If multiple timer blocks are active, use `--id=<id>` to specify which one.

## Config File

Optional TOML config at `$XDG_CONFIG_HOME/block-sleep/config.toml` (default: `~/.config/block-sleep/config.toml`).

Override with `--config=<path>` flag or `BLOCK_SLEEP_CONFIG_PATH` environment variable.

```toml
[default]
mode = "for"          # for, until, await, hold, forever
duration = "1h"       # for "for" mode
what = "sleep"

# Other mode examples:
# mode = "until"
# until = "22:00"
#
# mode = "await"
# await = "pgrep rsync"
# every = "30s"
#
# mode = "forever"
```

The config file is required for bare `block-sleep` (no arguments). If no config exists and no command is given, an error is shown.

## `--what` Types

The `--what` flag controls what is inhibited (default: `sleep`). Multiple types can be combined with colons.

| Type | Effect |
|------|--------|
| `sleep` | Prevent suspend/hibernate |
| `idle` | Prevent screen blanking/screensaver |
| `shutdown` | Prevent poweroff/reboot |
| `handle-lid-switch` | Prevent lid switch handling |

```sh
block-sleep for 2h --what=sleep:idle     # block both suspend and screen blanking
```

## Hidden Aliases

For those who prefer the gerund forms:

```sh
block-sleep doing -- make build     # same as: block-sleep run -- make build
block-sleep awaiting 1234           # same as: block-sleep await 1234
block-sleep holding /tmp/pipe       # same as: block-sleep hold /tmp/pipe
```

## Example Workflows

### Duration-based

```sh
# Block while watching a movie
block-sleep for 2h --what=sleep:idle

# Quick block while you step away
block-sleep for 30m

# Plain number = hours
block-sleep for 1.5              # 1 hour 30 minutes
```

### Deadline-based

```sh
# Block until tonight
block-sleep until 22:00

# Block until a specific date and time (overnight job)
block-sleep until 2026-04-01T06:00

# If it's past 22:00, "until 22:00" means tomorrow at 22:00
block-sleep until 22:00
```

### Wrapping a command

```sh
# Block while a backup runs, then automatically release
block-sleep run -- rsync -av --progress /data/ backup:/data/

# Block while building
block-sleep run -- make -j$(nproc) build

# Block while running a container
block-sleep run -- docker compose up

# Exit code is forwarded, so this works in scripts:
block-sleep run -- make test || echo "Tests failed"
```

### Watching a process

```sh
# Block while rsync is alive (started elsewhere)
rsync -av src/ dest/ &
block-sleep await $!

# Block while a specific process name is running
block-sleep await "pgrep -x firefox"

# Block while an SSH session is established
block-sleep await "ss -t state established | grep -q :22" --every=30s

# Block while a web service is healthy
block-sleep await "curl -sf http://localhost:8080/health" --every=10s

# Block while a VM is running
block-sleep await "virsh domstate myvm | grep -q running" --every=60s

# Block while audio is playing
block-sleep await "pactl list sinks | grep -q RUNNING" --every=15s

# Block while on AC power (polling; see `when` for an event-driven, re-arming version)
block-sleep await "cat /sys/class/power_supply/AC/online | grep -q 1" --every=60s
```

### Pipe-based signaling

```sh
# Block until explicitly signaled from another script/terminal
block-sleep hold /tmp/my-trigger

# ... in another terminal or script:
echo > /tmp/my-trigger           # releases the block

# Useful in cron/systemd scripts:
#   ExecStartPre=block-sleep hold /run/my-job-trigger &
#   ExecStopPost=echo > /run/my-job-trigger
```

### Indefinite

```sh
# Block forever (until explicit stop)
block-sleep forever

# ... later:
block-sleep stop
```

### Multiple concurrent blocks

```sh
$ block-sleep for 2h --what=sleep:idle
Sleep blocked for 2h00m (until 15:30) [#1]

$ block-sleep run -- make build        # in another terminal
Sleep blocked while running: make build [#2]

$ block-sleep await "pgrep rsync"
Sleep blocked while "pgrep rsync" succeeds (every 30s) [#3]

$ block-sleep status
#1  for (1h42m remaining, until 15:30) [sleep:idle]
#2  run (12m elapsed) [sleep]
#3  await cmd "pgrep rsync" (3m elapsed) [sleep]

# Stop just the timer block
$ block-sleep stop 1
Block #1 stopped.

# Extend doesn't need --id when only one timer block remains
$ block-sleep for 1h
Sleep blocked for 1h00m (until 16:15) [#4]

$ block-sleep extend 2h --id=4
Block #4 reset to 2h00m (until 17:15)

# Stop everything
$ block-sleep stop
3 sleep blocks stopped.
```

### Claude Code integration

Use a Claude Code hook to extend a timer block on each tool call:

```sh
# Start a block before your session:
block-sleep for 30m

# In your Claude Code hook (runs on each tool call):
block-sleep extend 30m
```

The block stays alive as long as Claude is active. When Claude stops, the block naturally expires after 30 minutes.

Alternative using `hold`:

```sh
# Hook on session start:
block-sleep hold /tmp/claude-block &

# Hook on session end:
echo > /tmp/claude-block
```

## How It Works

`block-sleep` starts a background daemon (except `run` mode, which is foreground) that:

1. Creates a named pipe (FIFO) in `$XDG_RUNTIME_DIR/block-sleep/`
2. Runs `sudo systemd-inhibit --what=<what> --mode=block cat <fifo>`
3. Waits on a mode-specific condition (timer, pidfd, poll, FIFO read, or forever)

The `cat` process holds the inhibit lock by blocking on the FIFO read end. When the daemon exits (for any reason), the FIFO closes, `cat` gets EOF, and `systemd-inhibit` releases the lock. Zero CPU usage while waiting.

Each block gets its own daemon, FIFO, and state file. Multiple blocks coexist naturally through `systemd-inhibit`'s native support for concurrent inhibitors.

- **stop** sends `SIGTERM` to the daemon (a user-space process, no sudo needed)
- **extend** updates the state file and sends `SIGUSR1` to wake the daemon, which resets its timer

`when` is different: its watcher runs as a user service and takes the block from logind over D-Bus, holding the returned file descriptor while armed and closing it to release. It sleeps on D-Bus signals, `SIGUSR1` (`recheck`), the grace timer and the optional `--every` ticker.

State is stored in `$XDG_RUNTIME_DIR/block-sleep/` (falls back to `/tmp/block-sleep/`).

## License

MIT
