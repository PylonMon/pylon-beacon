package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		latest, current string
		want            bool
	}{
		{"0.9.3", "0.9.2", true}, {"0.10.0", "0.9.9", true}, {"1.0", "0.9.9", true}, {"v0.9.3", "0.9.2", true},
		{"0.9.2", "0.9.2", false}, {"0.9.1", "0.9.2", false}, {"0.9", "0.9.0", false},
		{"0.9.3", "dev", false}, {"dev", "0.9.2", false}, {"", "0.9.2", false}, {"0.9.3-rc1", "0.9.2", false},
		{"<html>", "0.9.2", false},
	} {
		if got := newerVersion(c.latest, c.current); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestSumFor(t *testing.T) {
	sums := []byte("aaaa  other\n" + strings.Repeat("b", 64) + "  pylon-beacon-linux-amd64\n" + strings.Repeat("C", 64) + " *pylon-beacon-windows-amd64.exe\n")
	if s, ok := sumFor(sums, "pylon-beacon-linux-amd64"); !ok || s != strings.Repeat("b", 64) {
		t.Fatalf("linux: %q %v", s, ok)
	}
	if s, ok := sumFor(sums, "pylon-beacon-windows-amd64.exe"); !ok || s != strings.Repeat("c", 64) {
		t.Fatalf("binary-mode line / case: %q %v", s, ok)
	}
	if _, ok := sumFor(sums, "pylon-beacon-darwin-arm64"); ok {
		t.Fatal("found a checksum for an asset that is not listed")
	}
}

// fakeRelease serves VERSION, SHA256SUMS and this platform's asset.
func fakeRelease(t *testing.T, ver string, asset []byte, corruptSum bool) {
	t.Helper()
	sum := sha256.Sum256(asset)
	hexsum := hex.EncodeToString(sum[:])
	if corruptSum {
		hexsum = strings.Repeat("0", 64)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/") {
		case "VERSION":
			w.Write([]byte(ver + "\n"))
		case "SHA256SUMS":
			w.Write([]byte(hexsum + "  " + assetName() + "\n"))
		case assetName():
			w.Write(asset)
		default:
			http.NotFound(w, r)
		}
	}))
	oldBase, oldVer, oldProve := releaseBase, version, proveBinary
	releaseBase = srv.URL + "/"
	t.Cleanup(func() { srv.Close(); releaseBase, version, proveBinary = oldBase, oldVer, oldProve })
}

func TestStageUpdate(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "pylon-beacon.exe")
	os.WriteFile(exe, []byte("old"), 0o755)
	asset := []byte("the new binary")

	// a newer, intact release that proves itself: staged, the running binary untouched
	fakeRelease(t, "0.9.9", asset, false)
	version = "0.9.2"
	proved := ""
	proveBinary = func(path, want, cfg string) error { proved = path + "|" + want; return nil }
	st, err := stageUpdate(exe, "beacon.conf")
	if err != nil || st == nil || st.version != "0.9.9" {
		t.Fatalf("stage: %+v %v", st, err)
	}
	if b, _ := os.ReadFile(st.path); string(b) != string(asset) {
		t.Fatal("staged file is not the release asset")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatal("staging touched the running binary")
	}
	if !strings.HasSuffix(proved, "|0.9.9") {
		t.Fatalf("the candidate was not proven before staging: %q", proved)
	}
	os.Remove(st.path)

	// not newer -> nothing; a source build never updates itself
	version = "0.9.9"
	if st, err := stageUpdate(exe, "c"); st != nil || err != nil {
		t.Fatalf("same version staged: %+v %v", st, err)
	}
	version = "dev"
	if st, err := stageUpdate(exe, "c"); st != nil || err != nil {
		t.Fatalf("a dev build tried to update: %+v %v", st, err)
	}
}

func TestStageUpdateRefusesABadChecksum(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "pylon-beacon.exe")
	os.WriteFile(exe, []byte("old"), 0o755)

	fakeRelease(t, "0.9.9", []byte("tampered"), true)
	version = "0.9.2"
	proveBinary = func(string, string, string) error {
		t.Fatal("a binary with a bad checksum was executed")
		return nil
	}
	if st, err := stageUpdate(exe, "c"); st != nil || err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("bad checksum accepted: %+v %v", st, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*new*")); len(left) != 0 {
		t.Fatalf("a rejected download was left on disk: %v", left)
	}
}

func TestFailedProofLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "pylon-beacon.exe")
	os.WriteFile(exe, []byte("old"), 0o755)
	fakeRelease(t, "0.9.9", []byte("new but broken here"), false)
	version = "0.9.2"
	proveBinary = func(string, string, string) error { return os.ErrDeadlineExceeded }
	if st, err := stageUpdate(exe, "c"); st != nil || err == nil {
		t.Fatalf("a release that could not check in was staged: %+v %v", st, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*new*")); len(left) != 0 {
		t.Fatalf("a release that failed its proof was left on disk: %v", left)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatal("the running binary changed")
	}
}

func TestSwapBinaryKeepsThePreviousOne(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "pylon-beacon.exe")
	staged := filepath.Join(dir, "pylon-beacon.new.exe")
	os.WriteFile(exe, []byte("old"), 0o755)
	os.WriteFile(staged, []byte("new"), 0o755)
	oldVer := version
	version = "0.9.2"
	defer func() { version = oldVer }()
	kept, err := swapBinary(exe, staged)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatal("the new binary is not in place")
	}
	if b, _ := os.ReadFile(kept); string(b) != "old" || !strings.HasSuffix(kept, ".old-0.9.2") {
		t.Fatalf("previous binary not kept as .old-<version>: %s", kept)
	}
	// a swap that cannot complete changes nothing
	if _, err := swapBinary(exe, filepath.Join(dir, "does-not-exist")); err == nil {
		t.Fatal("swap with a missing staged file reported success")
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatal("a failed swap left the agent's binary missing or changed")
	}
}

func TestAutoUpdateIsOffByDefault(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "beacon.conf")
	os.WriteFile(p, []byte("key = k\nurl = http://x\n"), 0o644)
	cfg, err := loadConfig(p)
	if err != nil || cfg.AutoUpdate {
		t.Fatalf("auto-update must be OFF unless asked for: %+v %v", cfg, err)
	}
	os.WriteFile(p, []byte("key = k\nurl = http://x\nauto_update = true   # opt in\n"), 0o644)
	if cfg, _ := loadConfig(p); !cfg.AutoUpdate {
		t.Fatal("auto_update = true was not read")
	}
}
