package main

// autoupdate — opt-in self-update (auto_update = true).
//
// This code replaces a binary that runs as root / SYSTEM on someone else's
// machine, so every choice here is the conservative one:
//
//   - OFF unless the config says otherwise.
//   - The check and the download run in the BACKGROUND. A slow link must never
//     delay a check-in — a late check-in is a false page, and an updater that
//     pages the customer has failed at the only thing the agent is for.
//   - Nothing is replaced until the new binary has PROVED ITSELF on this
//     machine: its SHA-256 matches the release's SHA256SUMS, it reports the
//     version we expected, and it completes one real check-in with this
//     machine's own config (-once). A release that cannot check in from here
//     is never installed here.
//   - The swap itself happens between two check-ins and takes milliseconds;
//     the previous binary is kept beside the new one as <name>.old.
//   - Only ever forwards: a release that is not strictly newer is ignored, and
//     a build from source ("dev") never updates itself.
//
// Trust model, stated plainly: the release is fetched over HTTPS from this
// project's GitHub releases, and the checksum guards against a corrupt or
// truncated download. That is the same trust the install script already
// places in GitHub; it is not a signature.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// releaseBase is where a release's files live. A var so tests can point it at
// a local server.
var releaseBase = "https://github.com/PylonMon/pylon-beacon/releases/latest/download/"

const (
	updateEvery    = 6 * time.Hour
	updateMaxBytes = 64 << 20
)

// updateFirstCheck is how long after start the first check runs: let the node
// settle first. A string var ONLY so an end-to-end test build can shorten it
// with -ldflags "-X main.updateFirstCheck=5s"; there is deliberately no
// environment variable or config key for it (or for releaseBase) - nothing a
// running system can set should be able to redirect where updates come from.
var updateFirstCheck = "10m"

// updateReady holds the path of a verified, proven new binary waiting to be
// swapped in, or "" — set by the background updater, consumed by the push loop.
var updateReady atomic.Pointer[stagedUpdate]

// handedOver is set once this process has started its successor and is only
// waiting on it (Windows — see update_windows.go).
var handedOver atomic.Bool

type stagedUpdate struct{ path, version string }

func assetName() string {
	n := "pylon-beacon-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		n += ".exe"
	}
	return n
}

// newerVersion reports whether latest is strictly newer than current, comparing
// dotted numbers ("0.10.0" beats "0.9.2"). Anything unparseable is NOT newer.
func newerVersion(latest, current string) bool {
	parse := func(s string) ([]int, bool) {
		parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".")
		if len(parts) == 0 || len(parts) > 4 {
			return nil, false
		}
		out := make([]int, 0, len(parts))
		for _, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 {
				return nil, false
			}
			out = append(out, n)
		}
		return out, true
	}
	l, ok1 := parse(latest)
	c, ok2 := parse(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < len(l) || i < len(c); i++ {
		var a, b int
		if i < len(l) {
			a = l[i]
		}
		if i < len(c) {
			b = c[i]
		}
		if a != b {
			return a > b
		}
	}
	return false
}

var updateClient = &http.Client{Timeout: 5 * time.Minute}

func fetch(url string, max int64) ([]byte, error) {
	resp, err := updateClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", url, max)
	}
	return b, nil
}

// sumFor finds name's SHA-256 in a sha256sum-format listing.
func sumFor(sums []byte, name string) (string, bool) {
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return strings.ToLower(f[0]), true
		}
	}
	return "", false
}

// proveBinary runs the candidate: it must report the version we expect and
// complete one real check-in with this machine's config. A var for tests.
var proveBinary = func(path, wantVersion, cfgPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	out, err := exec.CommandContext(ctx, path, "-version").CombinedOutput()
	cancel()
	if err != nil {
		return fmt.Errorf("-version failed: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "pylon-beacon "+wantVersion {
		return fmt.Errorf("reports %q, expected version %s", got, wantVersion)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, path, "-config", cfgPath, "-once").CombinedOutput(); err != nil {
		return fmt.Errorf("could not check in (-once): %v: %s", err, lastLine(out))
	}
	return nil
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// stageUpdate does everything short of replacing the running binary. It
// returns ("", nil) when there is nothing newer.
func stageUpdate(exePath, cfgPath string) (*stagedUpdate, error) {
	if !newerVersion(version, "0") { // "dev", or anything else that is not a release number
		return nil, nil
	}
	raw, err := fetch(releaseBase+"VERSION", 64)
	if err != nil {
		return nil, err
	}
	latest := strings.TrimPrefix(strings.TrimSpace(string(raw)), "v")
	if !newerVersion(latest, version) {
		return nil, nil
	}
	sums, err := fetch(releaseBase+"SHA256SUMS", 1<<16)
	if err != nil {
		return nil, err
	}
	want, ok := sumFor(sums, assetName())
	if !ok {
		return nil, fmt.Errorf("release %s lists no checksum for %s", latest, assetName())
	}
	bin, err := fetch(releaseBase+assetName(), updateMaxBytes)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(bin)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("checksum mismatch for %s %s — refusing it", assetName(), latest)
	}
	staged := exePath + ".new"
	if runtime.GOOS == "windows" {
		staged = strings.TrimSuffix(exePath, ".exe") + ".new.exe" // Windows runs a file by its extension
	}
	if err := os.WriteFile(staged, bin, 0o755); err != nil {
		return nil, fmt.Errorf("cannot write beside the agent (%v) — auto-update needs write access to %s", err, filepath.Dir(exePath))
	}
	if err := proveBinary(staged, latest, cfgPath); err != nil {
		os.Remove(staged)
		return nil, fmt.Errorf("release %s did not pass its check on this machine, keeping %s: %v", latest, version, err)
	}
	return &stagedUpdate{path: staged, version: latest}, nil
}

// swapBinary puts the staged binary where the agent lives, keeping the old one
// beside it as <exe>.old-<version>. A running executable can be RENAMED on
// every platform we ship (on Windows it cannot be overwritten or deleted), so
// this is two renames; if the second fails the first is undone and nothing has
// changed. The old name carries the version because on Windows the previous
// .old may still be a RUNNING process (update_windows.go) and cannot be removed
// — a fixed name would make the second update of an uptime fail.
func swapBinary(exePath, staged string) (kept string, err error) {
	old := exePath + ".old-" + version
	os.Remove(old)
	if err := os.Rename(exePath, old); err != nil {
		return "", err
	}
	if err := os.Rename(staged, exePath); err != nil {
		if rerr := os.Rename(old, exePath); rerr != nil {
			return "", fmt.Errorf("%v — AND could not restore the previous binary: %v", err, rerr)
		}
		return "", err
	}
	return old, nil
}

// pruneOldBinaries keeps the newest previous binary (the rollback) and removes
// the rest. Best effort: one still running on Windows simply stays until the
// next start.
func pruneOldBinaries(exePath string) {
	olds, _ := filepath.Glob(exePath + ".old-*")
	if len(olds) < 2 {
		return
	}
	sort.Slice(olds, func(i, j int) bool {
		a, _ := os.Stat(olds[i])
		b, _ := os.Stat(olds[j])
		return a != nil && b != nil && a.ModTime().After(b.ModTime())
	})
	for _, f := range olds[1:] {
		os.Remove(f)
	}
}

// runUpdater is the background half. It never returns and never panics the
// agent: every failure is logged and tried again next round.
func runUpdater(cfgPath string) {
	exePath, err := os.Executable()
	if err == nil {
		exePath, err = filepath.EvalSymlinks(exePath)
	}
	if err != nil {
		log.Printf("auto-update: cannot locate the running binary (%v) — disabled", err)
		return
	}
	pruneOldBinaries(exePath)
	// jitter, so a fleet behind one address does not all fetch in one minute
	first, perr := time.ParseDuration(updateFirstCheck)
	if perr != nil || first <= 0 {
		first = 10 * time.Minute
	}
	time.Sleep(first + time.Duration(rand.Int63n(int64(first)*3+1)))
	for {
		if handedOver.Load() { // Windows: this process is now only the new agent's parent
			return
		}
		if updateReady.Load() == nil {
			st, err := stageUpdate(exePath, cfgPath)
			switch {
			case err != nil:
				log.Printf("auto-update: %v", err)
			case st != nil:
				log.Printf("auto-update: %s downloaded, verified and checked in from this machine — switching at the next check-in", st.version)
				updateReady.Store(st)
			}
		}
		time.Sleep(updateEvery + time.Duration(rand.Int63n(int64(time.Hour))))
	}
}

// applyStagedUpdate is the push loop's half: called BETWEEN check-ins, when a
// proven binary is waiting. On success it does not return (the process becomes
// the new agent, or hands over to it).
func applyStagedUpdate() {
	st := updateReady.Swap(nil)
	if st == nil {
		return
	}
	exePath, err := os.Executable()
	if err == nil {
		exePath, err = filepath.EvalSymlinks(exePath)
	}
	if err != nil {
		log.Printf("auto-update: %v — staying on %s", err, version)
		return
	}
	kept, err := swapBinary(exePath, st.path)
	if err != nil {
		log.Printf("auto-update: could not switch to %s (%v) — staying on %s", st.version, err, version)
		os.Remove(st.path)
		return
	}
	log.Printf("auto-update: %s -> %s (previous binary kept as %s)", version, st.version, filepath.Base(kept))
	if err := becomeNewAgent(exePath); err != nil {
		// the new binary is in place but could not be started from here; put the
		// old one back so the supervisor restarts what we know works
		log.Printf("auto-update: could not start %s (%v) — restoring %s", st.version, err, version)
		if rerr := errors.Join(os.Rename(exePath, st.path), os.Rename(kept, exePath)); rerr != nil {
			log.Printf("auto-update: restore failed: %v", rerr)
		}
	}
}

// parseBool accepts the spellings people actually type in a config file.
func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
