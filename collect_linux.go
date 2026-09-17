//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// previous CPU counters — cpu_pct is the busy fraction of the time between
// two gathers, so the very first push omits it.
var prevIdle, prevTotal uint64

func collect() map[string]any {
	m := map[string]any{}

	// cpu_pct — /proc/stat first line: cpu user nice system idle iowait irq softirq steal
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(line, "cpu ") {
				continue
			}
			f := strings.Fields(line)[1:]
			var total, idle uint64
			for i, s := range f {
				v, _ := strconv.ParseUint(s, 10, 64)
				total += v
				if i == 3 || i == 4 { // idle + iowait
					idle += v
				}
			}
			if prevTotal > 0 && total > prevTotal {
				dt, di := total-prevTotal, idle-prevIdle
				m["cpu_pct"] = round1(100 * float64(dt-di) / float64(dt))
			}
			prevIdle, prevTotal = idle, total
			break
		}
	}

	// mem_pct — MemTotal vs MemAvailable
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		var total, avail float64
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			v, _ := strconv.ParseFloat(f[1], 64)
			switch f[0] {
			case "MemTotal:":
				total = v
			case "MemAvailable:":
				avail = v
			}
		}
		if total > 0 {
			m["mem_pct"] = round1(100 * (total - avail) / total)
		}
	}

	// load1
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				m["load1"] = v
			}
		}
	}

	// uptime_s
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				m["uptime_s"] = float64(int64(v))
			}
		}
	}

	// temp_c — hottest thermal zone
	if zones, err := os.ReadDir("/sys/class/thermal"); err == nil {
		best := -1000.0
		for _, z := range zones {
			if !strings.HasPrefix(z.Name(), "thermal_zone") {
				continue
			}
			if b, err := os.ReadFile("/sys/class/thermal/" + z.Name() + "/temp"); err == nil {
				if v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil && v/1000 > best {
					best = v / 1000
				}
			}
		}
		if best > -1000 {
			m["temp_c"] = round1(best)
		}
	}

	// disk_pct per real mount
	real := map[string]bool{"ext4": true, "ext3": true, "ext2": true, "xfs": true,
		"btrfs": true, "zfs": true, "f2fs": true, "vfat": true, "ntfs": true, "exfat": true}
	if b, err := os.ReadFile("/proc/self/mounts"); err == nil {
		disks := map[string]any{}
		seen := map[string]bool{}
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 3 || !real[f[2]] || seen[f[1]] {
				continue
			}
			var st syscall.Statfs_t
			if syscall.Statfs(f[1], &st) != nil || st.Blocks == 0 {
				continue
			}
			used := 100 * (1 - float64(st.Bavail)/float64(st.Blocks))
			disks[f[1]] = round1(used)
			seen[f[1]] = true
			if len(disks) >= 8 {
				break
			}
		}
		if len(disks) > 0 {
			m["disk_pct"] = disks
		}
	}

	// net_rx_bps / net_tx_bps — bytes per second since the previous gather,
	// summed over real interfaces (no loopback), plus per-interface figures for
	// the ones carrying traffic. /proc/net/dev: iface: rx_bytes … (col 1)
	// … tx_bytes (col 9). First push omits it, like cpu_pct.
	if b, err := os.ReadFile("/proc/net/dev"); err == nil {
		now := time.Now()
		cur := map[string][2]uint64{}
		for _, line := range strings.Split(string(b), "\n") {
			name, rest, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			name = strings.TrimSpace(name)
			f := strings.Fields(rest)
			if name == "" || name == "lo" || len(f) < 9 || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "br-") {
				continue
			}
			rx, _ := strconv.ParseUint(f[0], 10, 64)
			tx, _ := strconv.ParseUint(f[8], 10, 64)
			cur[name] = [2]uint64{rx, tx}
		}
		netRates(m, cur, now)
	}
	return m
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
