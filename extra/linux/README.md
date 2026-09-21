# Running glean under systemd

`glean scan` is a batch job, not a daemon: it looks up whatever the schedule
says is due, writes the store and exits. To run it indefinitely, a timer starts
it again an hour after each run finishes. The first scan takes hours or days
and simply runs until it is done. After that, most runs take minutes.

| file             | installs to                              |
| ---------------- | ---------------------------------------- |
| `glean.service`  | `/etc/systemd/system/glean.service`      |
| `glean.timer`    | `/etc/systemd/system/glean.timer`        |
| `glean.sysusers` | `/etc/sysusers.d/glean.conf`             |

Paths, with the service's `XDG_STATE_HOME=/var/lib` and `XDG_CONFIG_HOME=/etc`:

| what        | where                                |
| ----------- | ------------------------------------ |
| store       | `/var/lib/glean/domains.jsonl.zst`   |
| RDAP cache  | `/var/lib/glean/rdap-bootstrap.json` |
| word lists  | `/etc/glean/sources.d/`              |

## Install

```
$ make build
# install -m 0755 glean /usr/local/bin/glean
# install -m 0644 extra/linux/glean.sysusers /etc/sysusers.d/glean.conf
# systemd-sysusers
# install -m 0644 extra/linux/glean.service extra/linux/glean.timer /etc/systemd/system/
# glean sources --init --sources /etc/glean/sources.d
# systemctl daemon-reload
# systemctl enable --now glean.timer
```

`sources --init` writes the starter word lists. Edit, add or switch them off
in `/etc/glean/sources.d/` at any time; the next run picks up the change.

## Watch it

```
$ systemctl list-timers glean.timer
$ journalctl -u glean.service -f
```

## Read the results

The store directory is readable by the `glean` group:

```
# usermod -aG glean $USER        # then log in again
$ glean list --store /var/lib/glean/domains.jsonl.zst --sources /etc/glean/sources.d
$ glean stats --store /var/lib/glean/domains.jsonl.zst --sources /etc/glean/sources.d
```

Reading while a scan is running is safe: the store is replaced by an atomic
rename, never rewritten in place.

## Tune it

Change the flags or the pause with a drop-in, not by editing the unit files:

```
# systemctl edit glean.service
[Service]
ExecStart=
ExecStart=/usr/local/bin/glean scan --limit 50000 --rate 2

# systemctl edit glean.timer
[Timer]
OnUnitInactiveSec=
OnUnitInactiveSec=6h
```

`--limit` caps each run and spends the lookups in source priority order.
Without it, each run keeps going until nothing is due.

## As a user service

Without root, `glean.timer` works as it is, and the service needs only this
much, in `~/.config/systemd/user/glean.service`:

```
[Unit]
Description=glean: look up every domain candidate that is due a check

[Service]
Type=oneshot
ExecStart=%h/go/bin/glean scan
TimeoutStopSec=5min
Nice=10
```

glean then uses its defaults, `~/.local/state/glean` and `~/.config/glean`.
Leave out the hardening block of the system unit: `ProtectHome=` would hide
exactly those directories. Enable the timer with
`systemctl --user enable --now glean.timer`, and run
`loginctl enable-linger $USER` so it keeps running when you are logged out.
