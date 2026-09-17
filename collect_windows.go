//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	pGlobalMemoryStatus = kernel32.NewProc("GlobalMemoryStatusEx")
	pGetTickCount64     = kernel32.NewProc("GetTickCount64")
	pGetSystemTimes     = kernel32.NewProc("GetSystemTimes")
	pGetDiskFreeSpaceEx = kernel32.NewProc("GetDiskFreeSpaceExW")
	pGetLogicalDrives   = kernel32.NewProc("GetLogicalDriveStringsW")
	pGetDriveType       = kernel32.NewProc("GetDriveTypeW")
)

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// previous CPU counters — cpu_pct is measured between two gathers, so the
// very first push omits it.
var prevIdleT, prevBusyT uint64

func fileTimeToU64(ft syscall.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

func collect() map[string]any {
	m := map[string]any{}

	// mem_pct — Windows hands us the percentage directly
	var ms memoryStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	if r, _, _ := pGlobalMemoryStatus.Call(uintptr(unsafe.Pointer(&ms))); r != 0 {
		m["mem_pct"] = float64(ms.MemoryLoad)
	}

	// uptime_s
	if t, _, _ := pGetTickCount64.Call(); t != 0 {
		m["uptime_s"] = float64(t / 1000)
	}

	// cpu_pct — GetSystemTimes deltas (idle vs kernel+user; kernel includes idle)
	var idleFT, kernFT, userFT syscall.Filetime
	if r, _, _ := pGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idleFT)),
		uintptr(unsafe.Pointer(&kernFT)),
		uintptr(unsafe.Pointer(&userFT))); r != 0 {
		idle := fileTimeToU64(idleFT)
		busy := fileTimeToU64(kernFT) + fileTimeToU64(userFT) // kernel includes idle
		if prevBusyT > 0 && busy > prevBusyT {
			db, di := busy-prevBusyT, idle-prevIdleT
			if db > 0 && di <= db {
				m["cpu_pct"] = round1(100 * float64(db-di) / float64(db))
			}
		}
		prevIdleT, prevBusyT = idle, busy
	}

	// disk_pct per fixed drive (C:\, D:\ …)
	buf := make([]uint16, 512)
	if n, _, _ := pGetLogicalDrives.Call(uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0]))); n > 0 {
		disks := map[string]any{}
		start := 0
		for i := 0; i < int(n); i++ {
			if buf[i] != 0 {
				continue
			}
			drive := syscall.UTF16ToString(buf[start:i])
			start = i + 1
			if drive == "" {
				continue
			}
			dp, _ := syscall.UTF16PtrFromString(drive)
			const driveFixed = 3
			if t, _, _ := pGetDriveType.Call(uintptr(unsafe.Pointer(dp))); t != driveFixed {
				continue
			}
			var freeToCaller, total, free uint64
			if r, _, _ := pGetDiskFreeSpaceEx.Call(
				uintptr(unsafe.Pointer(dp)),
				uintptr(unsafe.Pointer(&freeToCaller)),
				uintptr(unsafe.Pointer(&total)),
				uintptr(unsafe.Pointer(&free))); r != 0 && total > 0 {
				disks[drive] = round1(100 * (1 - float64(freeToCaller)/float64(total)))
			}
			if len(disks) >= 8 {
				break
			}
		}
		if len(disks) > 0 {
			m["disk_pct"] = disks
		}
	}
	// temp_c: not exposed by stable Win32 APIs — add one under [custom] if your
	// hardware vendor's CLI reports it.

	// net_rx_bps / net_tx_bps — `netstat -e` prints the interface totals
	// ("Bytes  <received> <sent>") in every Windows since XP, in ~50 ms; the
	// IP Helper table would need a struct per Windows version. Totals only
	// (no per-interface breakdown on Windows).
	if out, err := exec.Command("netstat", "-e").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) == 3 && strings.EqualFold(f[0], "Bytes") {
				rx, e1 := strconv.ParseUint(f[1], 10, 64)
				tx, e2 := strconv.ParseUint(f[2], 10, 64)
				if e1 == nil && e2 == nil {
					netRates(m, map[string][2]uint64{"all": {rx, tx}}, time.Now())
					delete(m, "net_if_rx_bps") // one pseudo-interface is not a breakdown
					delete(m, "net_if_tx_bps")
				}
				break
			}
		}
	}
	return m
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
