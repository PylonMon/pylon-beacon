// identity — a stable id for this machine, so the server can tell "the same
// node with a new name" from "a new node".
//
// Without one, PylonMon matched a check-in to its monitor by NAME alone, and
// any rename — uncommenting `node =` in the config, switching from the short
// hostname to the FQDN, fixing a typo — created a second monitor and orphaned
// the first, which then went quiet and paged. One customer hit this on day one
// (`kuruk` registered five times) and again two days later (`support` and
// `support.sterlingschool.org` for one box), and cleaned up the duplicates by
// hand each time.
//
// Sources, in order:
//
//	Linux    /etc/machine-id, then /var/lib/dbus/machine-id
//	Windows  HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid via `reg query`
//	macOS    IOPlatformUUID via `ioreg`
//	anything else, or any failure above: a UUID generated once and kept in
//	beacon.id beside the config file, where a reinstall that rewrites the
//	config leaves it alone.
//
// The raw value is never sent. It is hashed with its source, and the hash is
// what identifies the machine — systemd's own guidance is not to expose the
// machine-id directly, and the server needs stability, not the value.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// machineID returns the stable, hashed identity for this host. It never fails:
// the persisted-UUID fallback means there is always an answer, and a fresh
// UUID on a box that has lost its state file is the right answer ("this is a
// new install") rather than no answer.
func machineID(cfgPath string) string {
	if raw, src := platformID(); raw != "" {
		return hashID(src, raw)
	}
	return hashID("file", persistedID(cfgPath))
}

func hashID(src, raw string) string {
	sum := sha256.Sum256([]byte(src + ":" + strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:])[:24]
}

// platformID reads the OS's own machine identity. Empty when unavailable.
func platformID() (raw, src string) {
	switch runtime.GOOS {
	case "linux":
		for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
			if b, err := os.ReadFile(p); err == nil {
				if s := strings.TrimSpace(string(b)); len(s) >= 16 {
					return s, "machine-id"
				}
			}
		}
	case "windows":
		out, err := exec.Command("reg", "query",
			`HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if strings.Contains(line, "MachineGuid") {
					f := strings.Fields(line)
					if g := f[len(f)-1]; len(g) >= 32 {
						return g, "machineguid"
					}
				}
			}
		}
	case "darwin":
		out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if strings.Contains(line, "IOPlatformUUID") {
					if i := strings.LastIndex(line, "\""); i > 0 {
						if j := strings.LastIndex(line[:i], "\""); j >= 0 {
							return line[j+1 : i], "platformuuid"
						}
					}
				}
			}
		}
	}
	return "", ""
}

// persistedID returns the UUID kept beside the config, minting one on first
// use. Beside the config — not inside it — so an installer that rewrites
// beacon.conf does not also change the machine's identity.
func persistedID(cfgPath string) string {
	p := filepath.Join(filepath.Dir(cfgPath), "beacon.id")
	if b, err := os.ReadFile(p); err == nil {
		if s := strings.TrimSpace(string(b)); len(s) >= 32 {
			return s
		}
	}
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		// no entropy at all — fall back to the hostname rather than crash,
		// which is exactly today's (name-only) behaviour
		h, _ := os.Hostname()
		return "host:" + h
	}
	id := hex.EncodeToString(u[:])
	_ = os.WriteFile(p, []byte(id+"\n"), 0o600) // best-effort; unreadable next time = new id, not a crash
	return id
}
