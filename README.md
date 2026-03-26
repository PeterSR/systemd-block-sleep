# block-sleep

Temporarily block system sleep on Linux using `systemd-inhibit`. Useful when you want to prevent your laptop from sleeping for a set period (e.g., during a long download or build).

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

This is idempotent and writes `/etc/sudoers.d/block-sleep-<username>` with a rule scoped to `systemd-inhibit --what=sleep` only:

```
<username> ALL=(root) NOPASSWD: /usr/bin/systemd-inhibit --what=sleep --why=* --mode=block *
```

## Usage

```
block-sleep [duration]       Block sleep (default: 3h)
block-sleep status           Show remaining time
block-sleep extend <dur>     Reset the block to <dur> from now
block-sleep stop             Stop blocking sleep
block-sleep install-sudoers  Install sudoers file
block-sleep help             Show help
```

Duration can be a number (hours) or a Go duration string:

```sh
block-sleep              # 3 hours
block-sleep 2            # 2 hours
block-sleep 1.5          # 1 hour 30 minutes
block-sleep 45m          # 45 minutes
block-sleep 2h30m        # 2 hours 30 minutes
```

### Examples

```sh
$ block-sleep 2h
Sleep blocked for 2h00m (until 17:30)

$ block-sleep status
1h42m remaining (until 17:30)

$ block-sleep extend 3h
Sleep block reset to 3h00m (until 18:48)

$ block-sleep stop
Sleep block stopped.
```

## How it works

`block-sleep` starts a background daemon that:

1. Creates a named pipe (FIFO)
2. Runs `sudo systemd-inhibit --what=sleep --mode=block cat <fifo>`
3. Blocks on a kernel timer until the duration expires

The `cat` process holds the inhibit lock by blocking on the FIFO read end. The daemon holds the write end open while waiting on `select{timer, signal}`. Both processes are truly sleeping with zero CPU usage.

- **stop** sends `SIGTERM` to the daemon (a user-space process, no sudo needed). The FIFO closes, `cat` gets EOF, and `systemd-inhibit` releases the lock.
- **extend** updates a state file and sends `SIGUSR1` to wake the daemon, which resets its timer.

State is stored in `$XDG_RUNTIME_DIR/block-sleep.json` (falls back to `/tmp/`).

## License

MIT
