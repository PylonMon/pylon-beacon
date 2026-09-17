package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The identity has to be STABLE across calls and across config rewrites, and it
// must never contain the raw machine-id. Those are the properties the server
// relies on to treat a rename as an edit rather than a new node.
func TestMachineIDIsStableAndNeverRaw(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "beacon.conf")

	a := machineID(cfg)
	b := machineID(cfg)
	if a == "" || a != b {
		t.Fatalf("identity is not stable across calls: %q vs %q", a, b)
	}
	if len(a) != 24 {
		t.Fatalf("identity should be a 24-char hash, got %d chars", len(a))
	}
	// rewriting the config — what install.sh does on a reinstall — must not
	// change who this machine is
	if err := os.WriteFile(cfg, []byte("node = renamed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := machineID(cfg); c != a {
		t.Fatalf("a config rewrite changed the identity: %q -> %q", a, c)
	}
	// never the raw OS value
	if raw, _ := platformID(); raw != "" && a == raw {
		t.Fatal("the raw machine-id was sent instead of a hash")
	}
}

// With no OS identity the fallback UUID is persisted BESIDE the config, so a
// config rewrite leaves it alone — and a fresh directory gets a fresh id,
// which is the correct answer for "this is a new install".
func TestPersistedIDSurvivesConfigRewriteButNotReinstall(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "beacon.conf")
	a := persistedID(cfg)
	if len(a) < 32 {
		t.Fatalf("persisted id too short: %q", a)
	}
	if b := persistedID(cfg); b != a {
		t.Fatalf("persisted id not stable: %q vs %q", a, b)
	}
	if _, err := os.Stat(filepath.Join(dir, "beacon.id")); err != nil {
		t.Fatal("beacon.id was not written beside the config")
	}
	other := persistedID(filepath.Join(t.TempDir(), "beacon.conf"))
	if other == a {
		t.Fatal("two separate installs produced the same id")
	}
}

// The trailing-comment bug, verbatim: this exact line became a monitor name.
func TestTrailingCommentIsNotPartOfTheValue(t *testing.T) {
	for in, want := range map[string]string{
		"kuruk.avatarmc.pro        # uncomment to override the monitor name": "kuruk.avatarmc.pro",
		"web1.example.org # prod":  "web1.example.org",
		"web1.example.org\t; note": "web1.example.org",
		"abc#def":                  "abc#def",                  // a # inside a value is a value
		"https://x.test/page#frag": "https://x.test/page#frag", // URL fragment survives
		"p@ss#word":                "p@ss#word",                // so does a password
		"plain":                    "plain",
	} {
		if got := stripTrailingComment(in); got != want {
			t.Errorf("stripTrailingComment(%q) = %q, want %q", in, got, want)
		}
	}
}

// And through the real parser: the config line that bit the customer.
func TestParserDropsTrailingCommentOnNode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "beacon.conf")
	os.WriteFile(p, []byte("key = k\nnode = kuruk.avatarmc.pro        # uncomment to override the monitor name\n"), 0o600)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node != "kuruk.avatarmc.pro" {
		t.Fatalf("node = %q — the trailing comment leaked into the monitor name again", cfg.Node)
	}
}
