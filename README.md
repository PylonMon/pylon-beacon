# <picture><source media="(prefers-color-scheme: dark)" srcset="logo.svg"><img src="logo-light.svg" alt="" width="34" align="top"></picture> pylon-beacon

**One binary. No open ports. Full node vitals.**

pylon-beacon is a tiny monitoring agent for [PylonMon](https://pylonmon.com).
It **pushes** your machine's vitals out — CPU, memory, per-mount disk,
temperature, load, uptime, plus anything you add — so it works behind NAT,
CGNAT, and firewalls, on any box that can make an outbound HTTPS request.
Nothing scrapes you. Nothing listens. If the box goes silent, you get paged:
**the silence is the signal.**

- Single static binary (Go, standard library only — zero dependencies)
- Linux + Windows
- Auto-creates its own monitor in PylonMon on first push — no clicking
- Extensible: add your own metrics with one config line each

It's open source (MIT) and built to be read: the whole agent is about 3,400
lines of standard-library Go, so putting it on a machine you care about doesn't
mean trusting a black box. Contributions, bug reports, and "I couldn't monitor
my weird box" stories are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md).

## Contents

- [Install](#install)
- [The API key](#the-api-key)
- [Configuration](#configuration) — the basics, and `[custom]` metrics
- [Watching the LAN: `[probes]`](#watching-the-lan-probes) — http / tcp / ping from inside
- [Watching network gear: `[snmp]`](#watching-network-gear-snmp) — switches, firewalls, PDUs
- [Watching Proxmox: `[proxmox]`](#watching-proxmox-proxmox) — the whole cluster, one line
- [Watching logs: `[logwatch]`](#watching-logs-logwatch) — alert on an error *rate*
- [Shipping logs: `[logs]`](#shipping-logs-logs) — send the lines to PylonMon
- [Built-in collectors](#built-in-collectors)
- [What happens in PylonMon](#what-happens-in-pylonmon)
- [The wire format](#the-wire-format) — POST JSON yourself, no agent required
- [Service management](#service-management)
- [Contributing](#contributing)

## Install

**From source** (any platform with Go). This is the recommended route: the
whole agent is standard-library Go across a dozen files, so reading it before
you run it on your machines is actually realistic.

```sh
git clone https://github.com/PylonMon/pylon-beacon
cd pylon-beacon
go build -o pylon-beacon .
```

### Convenience installers

These fetch a shell script and execute it. That is a reasonable thing to be
wary of — read it first if you like:

```sh
curl -fsSL https://pylonmon.com/beacon.sh | less
```

**Linux** — installs `/usr/local/bin/pylon-beacon`, writes
`/etc/pylon-beacon.conf`, and registers a systemd unit:

```sh
curl -fsSL https://pylonmon.com/beacon.sh | sh
```

**Windows** — PowerShell as Administrator; installs to
`C:\Program Files\pylon-beacon` and registers a SYSTEM scheduled task that
starts at boot and auto-restarts:

```powershell
irm https://pylonmon.com/beacon.ps1 | iex
```

Both installers prompt for your API key (or read `PYLON_KEY` if set).

## The API key

Create an **🗼 ingest-scoped** key in PylonMon:
**Settings → Admin → Status page & API → + New key → scope "ingest"**.

Ingest keys can push check-ins and metrics and do **nothing else** — they
can't read your monitors, touch incidents, or change any settings. That makes
one key safe to distribute to every box you own.

## Configuration

Config lives at `/etc/pylon-beacon.conf` (Linux) or
`C:\Program Files\pylon-beacon\beacon.conf` (Windows). Full reference:

```ini
# ---- required ----
key      = pm_xxxxxxxxxxxx        # your ingest-scoped PylonMon API key

# ---- optional ----
url      = https://pylonmon.com   # your PylonMon instance
node     = db01                   # monitor name; defaults to this hostname
interval = 20                     # seconds between pushes (default 20, min 15)

# ---- extend what it collects ----
# Each entry under [custom] runs on every push. The FIRST NUMBER found in the
# command's output becomes the metric value, reported under the entry's name.
[custom]
gpu_temp_c    = nvidia-smi --query-gpu=temperature.gpu --format=csv,noheader
queue_depth   = redis-cli llen jobs
zpool_errors  = sh -c "zpool status -x | grep -c 'DEGRADED\|FAULTED'"
battery_pct   = powershell -c "(Get-CimInstance Win32_Battery).EstimatedChargeRemaining"
# is the LOCAL site up? 1/0 — pair with a vital rule `site_up < 1`
site_up       = sh -c "curl -sf http://localhost/ >/dev/null && echo 1 || echo 0"
```

**Monitoring a web server?** For *public* up/down, a normal off-site PylonMon
HTTP monitor is the better tool — it sees what your visitors see. Use the
`site_up` recipe above for a *local* check the outside world can't do (is the
app answering on localhost even when the public route is broken?), then set a
vital rule `site_up < 1` on the node monitor.

Notes on `[custom]`:

- Commands run as the service user, with the agent's environment.
- Anything printable-as-a-number works: shell one-liners, scripts, CLIs.
- A failing command just skips that metric for the push — it never blocks
  the heartbeat.
- Name metrics with units in the suffix (`_pct`, `_c`, `_s`) and PylonMon
  formats them accordingly.

## Watching the LAN: `[probes]`

PylonMon's own pollers live on the public internet — that's the point of
off-site monitoring, and exactly why they can never reach the NAS at
192.168.x, the hypervisor's UI, or anything else that rightly refuses to face
the internet. The beacon already lives inside. `[probes]` has it check those
targets from where it stands and ship each result with its push — every probe
becomes its own monitor in PylonMon, with real measured latency, incidents
and alerts.

```ini
[probes]
# name = kind target [timeout-seconds]     kinds: http, tcp, ping
pihole   = http http://192.168.1.240/admin
nas_smb  = tcp 192.168.1.50:445
gateway  = ping 192.168.1.1
slow_box = http https://10.0.0.9:8443 10
```

Notes:

- **Nothing is exposed.** Probes run from the beacon outward; the results
  leave over the same outbound push as everything else. No listening socket,
  no tunnel, no reverse proxy.
- **http** counts anything below 500 as up — a 401 from a NAS login page is a
  living service. Self-signed certificates are accepted: a probe asserts
  liveness, not identity.
- **tcp** is a plain connect. **ping** uses the system ping tool, so the
  agent needs no special privileges.
- Timeout defaults to 5 seconds. A probe that times out reports **down at
  the timeout** — it never silently vanishes.
- Probes run concurrently and are bounded by the push cycle, so a dark LAN
  can never delay a check-in. Up to 20 per agent.
- Each probe appears in PylonMon as its own monitor under this node and
  inherits the node's alert channels. If the agent itself goes quiet, the
  node's silence page covers everything — probes go stale rather than paging
  twenty times.

## Watching network gear: `[snmp]`

Switches, firewalls, APs, NAS boxes, PDUs and printers rarely run an agent —
but almost all of them speak SNMP. Point `[snmp]` at one and its readings become
ordinary metrics, with the same thresholds, alerts and incidents as CPU or disk.

```ini
[snmp]
target    = 192.168.1.1     # host or host:port (default 161)
community = public          # v2c read community
timeout   = 3               # seconds per poll

# Every other key is a metric name = the OID to read.
uptime_ticks   = .1.3.6.1.2.1.1.3.0          # sysUpTime
wan_in_octets  = .1.3.6.1.2.1.2.2.1.10.1     # ifInOctets on interface 1
wan_out_octets = .1.3.6.1.2.1.2.2.1.16.1     # ifOutOctets on interface 1
wan_oper       = .1.3.6.1.2.1.2.2.1.8.1      # ifOperStatus: 1 = up
```

**`snmp_up` is always reported — 1 or 0.** This is the important bit. A metric
that simply *stops arriving* does not alert: PylonMon skips a vital rule whose
metric is missing from a push. So if the device dies, its readings vanish and
nothing pages you. `snmp_up` turns that silence into a value you can alert on —
set a vital rule **`snmp_up < 1`** and you'll know.

Notes:

- **SNMPv2c only**, read-only, and the beacon only ever issues GETs — it can
  never write to your gear.
- The polling happens **on your LAN**, from the beacon. Nothing is exposed to
  the internet; only the results go out, over the same outbound push as
  everything else. That's the difference from every other SNMP tool, which
  wants to reach *into* your network.
- Non-numeric answers (device name, location) are ignored — they're not metrics.
- A missing OID is reported as *nothing*, never as `0`. A device that doesn't
  implement an OID shouldn't look like a genuine reading of zero.
- Enable SNMP on the device first. On UniFi that's
  **Settings → System → Advanced → SNMP** (availability varies by UniFi OS
  version). Vendor-specific OIDs live in that vendor's MIB; the standard
  MIB-II OIDs above work on essentially anything.

## Watching Proxmox: `[proxmox]`

On a hypervisor, the host is the least interesting half of the picture. It can
sit at 4% CPU with plenty of free memory while a VM has been off since Tuesday.
Add one section on the Proxmox host and the agent reports the *cluster's*
inventory alongside the host's own vitals:

```ini
[proxmox]
enabled = true
```

That's the whole configuration. The agent runs as root on the host, so it uses
the `pvesh` CLI that's already installed and already authenticated — no API
token to create, store, or rotate. It works the same on a standalone host as on
a cluster; Proxmox reports a single-node "cluster".

| Metric | What it is |
|---|---|
| `pve_qemu_running` / `_stopped` / `_total` | VMs |
| `pve_lxc_running` / `_stopped` / `_total` | LXC containers |
| `pve_guests_stopped` | every non-template guest that isn't running |
| `pve_nodes_online` / `_total` | cluster members |
| `pve_storage_pct` | the **fullest** storage, not an average |
| `pve_quorate` | `1` with quorum, `0` without (clusters only) |

Then a PylonMon vital rule does the judging — for example `pve_guests_stopped`
above `0` for 5 minutes, or `pve_storage_pct` above `85`. When guests are down,
the push also carries the *names* (`vm/102 db-01 on pve1`), so the alert tells
you what to go and start rather than just how many.

Notes:

- **Templates are never counted as stopped.** A template's status is `stopped`
  forever by definition; counting them would page every user who has one.
- **`pve_storage_pct` is the fullest pool**, deliberately. An average across one
  full pool and three empty ones hides the one that's about to break backups.
- Counters are reported at `0` when healthy, not omitted — a metric that only
  appears once something is wrong can't be graphed, and a threshold on a metric
  that's never been seen is never evaluated.
- If `pvesh` is missing, times out, or returns something unrecognisable, the
  integration reports **nothing** rather than zeros. "No data" and "everything
  is gone" must not look the same.
- `timeout` (seconds, default 8) and `bin` (path to `pvesh`) are available if
  you need them.

## Watching logs: `[logwatch]`

Some problems are a *rate*, not a state: a few Python tracebacks an hour is
life on the internet; forty in five minutes is an incident. Each `[logwatch]`
entry tails a file, counts regex matches inside a rolling window, and reports
the count as a normal metric — so a PylonMon vital rule with a sustained gate
does the judging. The latest matched block (a whole traceback, not just the
first line) rides along with the push, so a breach page can show you the
actual error.

```ini
# name = <file> | <regex, Go syntax, matched per line> | <window seconds>
[logwatch]
tracebacks_5m = /var/log/app/app.log | Traceback \(most recent call last\): | 300
oom_kills     = /var/log/kern.log    | Out of memory | 3600
http_500s     = /var/log/nginx/access.log | " 500 | 300
```

Then on the node monitor in PylonMon: `tracebacks_5m > 20 sustained 60s` →
the page arrives with the count *and* the most recent traceback.

Notes on `[logwatch]`:

- Reading is incremental — only what the file grew since the last push is
  scanned, capped at 4 MB per cycle so a log flood can't stall the beacon.
- The file may be a **glob** (`/var/lib/docker/containers/*/*.log`),
  re-expanded every cycle — matches count across every file it finds, so
  container churn and rotation don't lose the thread.
- On first start (and for each file a glob newly finds) the watch begins at
  the **end** of the file: installing the beacon never pages you about last
  week's errors.
- Truncation and rotation are detected (file shrank) and the watch restarts
  from the top of the new file.
- Block capture is traceback-shaped: the matched line, its indented
  continuation lines, and the first non-indented line after them (the
  `SomeError: message` at the bottom), capped at 40 lines / 4 KB.
- The window defaults to 300s; minimum 15s. Name entries like metrics
  (`_5m`, `_1h` suffixes read well on the dashboard).

## Shipping logs: `[logs]`

`[logwatch]` counts error patterns so you can alert on a spike. `[logs]` ships
the lines themselves, so they show up in PylonMon's **Logs → Log streams**
search (live tail included) and on every **incident** — the log lines from
around the moment things broke, on the same screen that paged you.

```ini
[logs]
# name = <file-or-glob> | <service> | <filter-regex>   (service and filter optional)
app  = /var/log/myapp/*.log | myapp
# Kubernetes / containerd: one entry covers every pod on the node.
# Service names come from the CRI filename (pod_namespace_container).
k8s  = /var/log/containers/*.log
# A filter ships only matching lines — for files where one line in a
# thousand is worth keeping. It's the LAST field, so `|` alternation
# inside the regex just works. Leave the service empty (`| |`) to filter
# while still deriving the service name from the file.
ssh  = /var/log/auth.log | sshd | Failed password|Invalid user|authentication failure
```

- Globs re-expand every cycle, so new pods and rotated files are picked up.
- The filter is matched against the line **after** container unwrapping — the
  same text you'd see in the portal, not the JSON envelope.
- Docker `json-file` and Kubernetes CRI line formats are unwrapped
  automatically — you get the message, not the envelope — and their
  runtime timestamps are honored.
- A light guess at the level (`ERROR`/`warn`/`panic`…) makes PylonMon's
  severity filters useful; JSON logs with a `level` field are read as-is.
- Same discipline as `[logwatch]`: first sight starts at the **end** of the
  file, rotation restarts from the top, and reads are capped per cycle so a
  log flood can't eat the box.
- Delivery is deliberately lossy under pressure: lines go out once per
  check-in interval in one bounded batch, and if the server is shedding (your
  plan's daily cap, or a flood) the batch is dropped rather than retried —
  paging always outranks log intake.

Plan limits (retention and daily volume) are on the
[pricing page](https://pylonmon.com/#pricing).

## Built-in collectors

| Metric        | Linux source              | Windows source            |
|---------------|---------------------------|---------------------------|
| `cpu_pct`     | `/proc/stat` delta        | `GetSystemTimes` delta    |
| `mem_pct`     | `/proc/meminfo`           | `GlobalMemoryStatusEx`    |
| `disk_pct` (per mount) | `statfs`         | `GetDiskFreeSpaceEx`      |
| `load1`       | `/proc/loadavg`           | —                         |
| `temp_c`      | `/sys/class/thermal`      | — (add via `[custom]`)    |
| `uptime_s`    | `/proc/uptime`            | `GetTickCount64`          |

## What happens in PylonMon

- The first push **auto-creates a node monitor** named after the machine — a
  first-class **BEACON** monitor type. It counts as one monitor against your plan.
- Deadline = **1.5× your push interval** (minimum 30s), so the default 20s
  agent is flagged DOWN after ~30s of silence — alert channels, escalation
  ladders, and incidents all fire like any PylonMon monitor. Tune it per node
  in the monitor's edit form.
- The monitor's detail card shows the live **🗼 Node vitals** grid — with
  per-vital sparklines from recent pushes and daily min/avg/max trends kept
  for your plan's retention window.
- **Threshold rules**: on the node monitor you can set per-vital alerts
  (`disk_pct / > 90`, `temp_c > 75`, …) that page your channels the moment a
  push crosses the line — and send an all-clear when it recovers.
- Attach runbooks, labels, and SLOs like any other monitor.

## The wire format

The agent is a convenience, not a requirement. Anything that can POST JSON
over HTTPS can be a beacon:

```sh
curl -X POST https://pylonmon.com/api/ingest/exporter \
  -H "Authorization: Bearer $PYLON_KEY" \
  -d '{"node":"db01","interval":60,"metrics":{"cpu_pct":12.3,"mem_pct":44.1,"disk_pct":{"/":81},"temp_c":52}}'
```

`metrics` accepts numbers and one level of nesting (for per-mount disk and
the like). Up to 40 metrics per push.

`[probes]` results ride as an optional top-level `probes` array —
`{"probes":[{"name":"pihole","kind":"http","target":"http://…","ok":true,"ms":12}]}` —
one entry per probe per push. Servers that predate probes ignore the field.

`[logwatch]` entries add an optional top-level `samples` object —
`{"samples":{"tracebacks_5m":"Traceback (most recent call last):
…"}}` — with
the latest matched block per watch (≤4 KB each). Servers that predate samples
simply ignore the field.

## Service management

Linux:

```sh
systemctl status pylon-beacon
journalctl -u pylon-beacon -f
```

Windows (PowerShell as admin):

```powershell
Get-Service pylon-beacon
Restart-Service pylon-beacon
```

## Contributing

Pull requests, issues, and questions are all welcome — you don't need to ask
first. The project is small and dependency-free on purpose, so reading it before
you change it is realistic. [CONTRIBUTING.md](CONTRIBUTING.md) has the build and
test commands, a map of the files, and the handful of design principles the code
already follows (the big one: *the silence is the signal* — nothing may delay a
check-in).

## License

MIT.
