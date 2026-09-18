// pylon-beacon — one binary, no open ports, full node vitals.
//
// Collects this machine's vitals (CPU, memory, per-mount disk, temperature,
// load, uptime — plus anything you add under [custom]) and PUSHES them to
// PylonMon over outbound HTTPS. Nothing listens, nothing is scraped: it works
// behind NAT, CGNAT, and firewalls. If the box goes silent, PylonMon pages
// you — the silence is the signal.
//
// Standard library only. Config: /etc/pylon-beacon.conf (Linux) or
// beacon.conf next to the executable (Windows). See README.md.
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// version is stamped by the release build from the git tag
// (-ldflags "-X main.version=..."; see .github/workflows/release.yml), so it
// cannot drift from the release it shipped in. It used to be a constant edited
// by hand, and nobody edited it: v0.8.0 and v0.9.0 both reported 0.7.3, so a
// customer who updated saw no change on their dashboard. A build from source
// says "dev".
var version = "dev"

type config struct {
	Key      string
	URL      string
	Node     string
	ID       string // stable machine identity (identity.go) — never from the config file
	Interval int
	Template string            // which beacon template seeds this node's monitor (name or id); optional
	Custom   map[string]string // metric name -> command
	Logwatch []*logWatch       // [logwatch] entries — see logwatch.go
	Logs     []*logShip        // [logs] entries — see logship.go
	SNMP     *snmpConfig       // [snmp] section — see snmp.go
	Proxmox  *proxmoxConfig    // [proxmox] section — see proxmox.go
	Probes   []*probe          // [probes] section — see probes.go
}

func defaultConfigPath() string {
	if runtime.GOOS == "windows" {
		exe, err := os.Executable()
		if err == nil {
			return filepath.Join(filepath.Dir(exe), "beacon.conf")
		}
		return "beacon.conf"
	}
	return "/etc/pylon-beacon.conf"
}

// stripTrailingComment drops a " # …" or " ; …" tail from a config value. The
// marker must follow whitespace: `abc#def` is a value, `abc # def` is a value
// with a comment.
func stripTrailingComment(v string) string {
	for _, m := range []string{" #", "\t#", " ;", "\t;"} {
		if i := strings.Index(v, m); i >= 0 {
			v = v[:i]
		}
	}
	v = strings.TrimSpace(v)
	// `node = "SCS Print 1"` — people quote values with spaces out of habit,
	// and the quotes became part of the monitor name. Spaces need no quotes;
	// matching surrounding quotes are dropped.
	if n := len(v); n >= 2 && (v[0] == '"' && v[n-1] == '"' || v[0] == '\'' && v[n-1] == '\'') {
		v = strings.TrimSpace(v[1 : n-1])
	}
	return v
}

// loadConfig parses the ini-ish config: `key = value` lines, comments with #,
// and a [custom] section whose entries are metric-name = command.
func loadConfig(path string) (*config, error) {
	cfg := &config{URL: "https://pylonmon.com", Interval: 20, Custom: map[string]string{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	section := ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.Trim(line, "[]"))
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		// A trailing comment is not part of the value. This is how
		// `node = kuruk.avatarmc.pro   # uncomment to override the monitor name`
		// became a monitor literally named that — and how the documented
		// `pihole = http http://… # up = any response` failed as a bad timeout.
		// Only a marker that FOLLOWS whitespace counts, so a # inside a value
		// (URL fragment, password) is left alone. [custom] and [logwatch] are
		// never touched: shell commands and regexes, where ` ; ` and `#` are code.
		switch section {
		case "custom", "logwatch": // free text — leave exactly as written
		default:
			v = stripTrailingComment(v)
		}
		if section == "custom" {
			if k != "" && v != "" {
				cfg.Custom[sanitizeMetricName(k)] = v
			}
			continue
		}
		if section == "snmp" {
			if k == "" || v == "" {
				continue
			}
			if cfg.SNMP == nil {
				cfg.SNMP = &snmpConfig{OIDs: map[string]string{}}
			}
			// A few reserved keys configure the connection; every OTHER key is
			// a metric name whose value is the OID to read.
			switch strings.ToLower(k) {
			case "target", "host", "address":
				cfg.SNMP.Target = v
			case "community":
				cfg.SNMP.Community = v
			case "timeout":
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					cfg.SNMP.TimeoutMS = n * 1000
				}
			case "version":
				if v != "2c" && v != "v2c" && v != "2" {
					fmt.Fprintln(os.Stderr, "snmp: only v2c is supported (got "+v+"); continuing as v2c")
				}
			default:
				cfg.SNMP.OIDs[sanitizeMetricName(k)] = v
			}
			continue
		}
		if section == "probes" {
			if k == "" || v == "" {
				continue
			}
			p, perr := parseProbe(sanitizeMetricName(k), v)
			if perr != nil {
				fmt.Fprintln(os.Stderr, "probe "+k+": "+perr.Error()+" (entry skipped)")
				continue
			}
			if len(cfg.Probes) >= probeCap {
				fmt.Fprintln(os.Stderr, "probe "+k+": over the "+strconv.Itoa(probeCap)+"-probe limit (entry skipped)")
				continue
			}
			cfg.Probes = append(cfg.Probes, p)
			continue
		}
		if section == "logwatch" {
			if k == "" || v == "" {
				continue
			}
			w, werr := parseLogwatch(sanitizeMetricName(k), v)
			if werr != nil {
				fmt.Fprintln(os.Stderr, "logwatch "+k+": "+werr.Error()+" (entry skipped)")
				continue
			}
			cfg.Logwatch = append(cfg.Logwatch, w)
			continue
		}
		if section == "logs" {
			if k == "" || v == "" {
				continue
			}
			ls, lerr := parseLogship(k, v)
			if lerr != nil {
				fmt.Fprintln(os.Stderr, "logs "+k+": "+lerr.Error()+" (entry skipped)")
				continue
			}
			cfg.Logs = append(cfg.Logs, ls)
			continue
		}
		// [proxmox] — reports the hypervisor's guests, storage and quorum, not
		// just the host it runs on. The whole section is usually two words:
		//
		//	[proxmox]
		//	enabled = true
		if section == "proxmox" {
			if k == "" {
				continue
			}
			if cfg.Proxmox == nil {
				cfg.Proxmox = &proxmoxConfig{}
			}
			switch strings.ToLower(k) {
			case "enabled", "on":
				cfg.Proxmox.Enabled = truthy(v)
			case "pvesh", "bin":
				cfg.Proxmox.Bin = v
			case "timeout":
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					cfg.Proxmox.TimeoutMS = n * 1000
				}
			default:
				fmt.Fprintln(os.Stderr, "proxmox: unknown setting "+k+" (ignored)")
			}
			continue
		}
		switch strings.ToLower(k) {
		case "key":
			cfg.Key = v
		case "url":
			cfg.URL = strings.TrimRight(v, "/")
		case "node":
			cfg.Node = v
		case "template":
			cfg.Template = v
		case "interval":
			if n, err := strconv.Atoi(v); err == nil {
				cfg.Interval = n
			}
		}
	}
	return cfg, nil
}

func sanitizeMetricName(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.Join(strings.Fields(s), "_")
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}

// truthy accepts the spellings people actually type in a config file. Anything
// else is false, so a typo turns a section OFF rather than silently on.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "enabled":
		return true
	}
	return false
}

var numRe = regexp.MustCompile(`-?\d+(\.\d+)?`)

// runCustom executes one [custom] command and returns the first number in its
// output. A failing or numberless command skips the metric — never the push.
func runCustom(command string) (float64, bool) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return 0, false
	}
	if err != nil && len(out) == 0 {
		return 0, false
	}
	m := numRe.Find(out)
	if m == nil {
		return 0, false
	}
	f, perr := strconv.ParseFloat(string(m), 64)
	return f, perr == nil
}

func push(cfg *config, metrics map[string]any, samples map[string]string, probes []probeResult, timeout time.Duration, fresh bool) error {
	payload := map[string]any{
		"node": cfg.Node, "interval": cfg.Interval, "metrics": metrics,
		// stable identity, so a renamed node edits its monitor instead of
		// spawning a duplicate (identity.go); older servers ignore it
		"id": cfg.ID,
	}
	if cfg.Template != "" {
		// only matters on the node's first check-in, when its monitor is
		// created; older servers ignore the field
		payload["template"] = cfg.Template
	}
	if len(samples) > 0 {
		// text samples captured by [logwatch] — servers that predate them
		// simply ignore the field
		payload["samples"] = samples
	}
	if len(probes) > 0 {
		// [probes] results — same deal, older servers ignore the field
		payload["probes"] = probes
	}
	body, _ := json.Marshal(payload)
	if fresh {
		// second attempt: never reuse the connection the first one failed on
		pushTransport.CloseIdleConnections()
	}
	err := postCheckIn(cfg, body, gzipOK.Load(), timeout)
	var rejected *gzipRejected
	if errors.As(err, &rejected) {
		// a server that predates compressed check-ins: say so once, stop
		// compressing for the life of this process, and send this one plain
		log.Printf("server did not accept a compressed check-in (%v) — sending uncompressed from now on", err)
		gzipOK.Store(false)
		err = postCheckIn(cfg, body, false, timeout)
	}
	return err
}

// pushTransport carries every check-in. Two deliberate choices, both from a
// customer's domain controller whose check-ins kept reaching the server as
// headers with NO body (2026-09-18) — the node was paged as silent while it was
// up and pushing:
//
//   - HTTP/1.1 only. Over HTTP/2 the headers and the body are separate frames,
//     so a request can be delivered in halves. Over HTTP/1.1 Go writes the
//     headers and a small body in ONE flush: it arrives whole or not at all.
//   - which is why the body is gzipped (postCheckIn): a check-in compresses to
//     a few hundred bytes, so headers + body fit in a single small packet —
//     well under any path's MTU, and nothing for a middlebox to cut in two.
//
// One shared transport also keeps the TLS session warm between check-ins, so a
// busy machine does not pay a full handshake inside every attempt's deadline.
var pushTransport = &http.Transport{
	Proxy:               http.ProxyFromEnvironment,
	TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{}, // non-nil and empty = no HTTP/2
	TLSHandshakeTimeout: 8 * time.Second,
	IdleConnTimeout:     90 * time.Second,
	MaxIdleConnsPerHost: 2,
}

// gzipOK turns false the first time a server refuses a compressed check-in.
var gzipOK = func() *atomic.Bool { b := &atomic.Bool{}; b.Store(true); return b }()

// gzipRejected marks a refusal that is about the ENCODING, not the check-in.
type gzipRejected struct{ msg string }

func (g *gzipRejected) Error() string { return g.msg }

func postCheckIn(cfg *config, body []byte, compress bool, timeout time.Duration) error {
	send := body
	if compress {
		var zb bytes.Buffer
		zw := gzip.NewWriter(&zb)
		zw.Write(body)
		zw.Close()
		send = zb.Bytes()
	}
	req, err := http.NewRequest("POST", cfg.URL+"/api/ingest/exporter", bytes.NewReader(send))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Key)
	req.Header.Set("Content-Type", "application/json")
	if compress {
		req.Header.Set("Content-Encoding", "gzip")
	}
	req.Header.Set("User-Agent", "pylon-beacon/"+version+" ("+runtime.GOOS+")")
	resp, err := (&http.Client{Timeout: timeout, Transport: pushTransport}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		msg := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, e.Error)
		if compress && (resp.StatusCode == 400 || resp.StatusCode == 415) {
			return &gzipRejected{msg}
		}
		return errors.New(msg)
	}
	return nil
}

// pushBudget is how long delivering one check-in may take in total, across
// both attempts. Collection for the NEXT check-in happens during the wait, so
// this time comes out of the period: half of it at most, capped.
//
// It used to be half the period capped at 10s — 5s per attempt — and 5s turned
// out to be too little for a machine under pressure. A customer's domain
// controller at 99% memory (2026-09-18) was measured taking 2.4s just between
// writing a request's headers and its 185-byte body; add a TLS handshake and
// the attempt died mid-request, twice, and the node was paged as silent while
// it was up. 7.5s per attempt at a 30s interval, and the transport now keeps the
// TLS session warm so the handshake is not paid every time.
func pushBudget(period time.Duration) time.Duration {
	b := period / 2
	if b > 16*time.Second {
		b = 16 * time.Second // long intervals do not need a long deadline
	}
	if b < 500*time.Millisecond {
		b = 500 * time.Millisecond
	}
	return b
}

// pushAttempts is deliberately small. The point is to survive a BLIP, not to
// hammer a server that is genuinely down — if it is down, the silence is the
// signal and that is the product working.
const pushAttempts = 2

// sendCheckIn delivers one check-in, retrying briefly instead of surrendering
// the whole interval.
//
// WHY. A single failed push used to cost a full period, because the loop just
// logged it and waited for the next tick. On a 20s beacon against a 30s
// silence window that turns one transient failure into a 40s+ gap — which is a
// page. Over seven days across three nodes: 17 failures, and today one of them
// paged the founder at 13:00. The check-in HAD been received; the server was
// simply too slow to answer, so a delivery that actually landed was thrown
// away and the node reported silent.
//
// Retrying inside the interval turns that into a sub-second hiccup. Each
// attempt gets half the budget, so two attempts fit with room to spare, and a
// normal push takes ~200ms against a multi-second deadline — 20x headroom, so
// a merely slow server is not mistaken for a failed one.
//
// Re-sending a check-in the server already recorded is harmless: ingest is an
// upsert of the node's vitals, so the duplicate just refreshes last_ping.
func sendCheckIn(cfg *config, period time.Duration, metrics map[string]any, samples map[string]string, probes []probeResult) {
	per := pushBudget(period) / pushAttempts
	var err error
	for i := 1; i <= pushAttempts; i++ {
		if err = push(cfg, metrics, samples, probes, per, i > 1); err == nil {
			if i > 1 {
				log.Printf("pushed %d metric(s) (attempt %d)", len(metrics), i)
			} else {
				log.Printf("pushed %d metric(s)", len(metrics))
			}
			return
		}
		if i < pushAttempts {
			log.Printf("push attempt %d/%d failed, retrying: %v", i, pushAttempts, err)
		}
	}
	log.Printf("push failed after %d attempts (will retry next interval): %v", pushAttempts, err)
}

// runCustomAll executes every [custom] command CONCURRENTLY and returns the
// metrics that produced a number.
//
// They used to run one after another. Each command is capped at 10 seconds
// (runCustom), so the collection phase was bounded only by 10s x however many
// commands you configured — with seven of them, 70 seconds, which is longer
// than any sane interval. Collection time is dead time: it is time the node is
// not checking in, and the server is judging silence.
//
// Concurrently, the phase costs the SLOWEST command rather than their sum, and
// the whole phase is bounded by deadline so one wedged command cannot push the
// cycle past the interval either. Commands are independent by definition — each
// one's first number becomes one metric — so nothing depends on their order.
//
// Concurrency is capped so a config with many commands cannot fork a hundred
// shells at once on a small box.
func runCustomAll(custom map[string]string, deadline time.Duration) map[string]float64 {
	out := map[string]float64{}
	if len(custom) == 0 {
		return out
	}
	const maxParallel = 4
	type res struct {
		name string
		v    float64
		ok   bool
	}
	ch := make(chan res, len(custom))
	slots := make(chan struct{}, maxParallel)
	for name, command := range custom {
		go func(name, command string) {
			slots <- struct{}{}
			defer func() { <-slots }()
			v, ok := runCustom(command)
			ch <- res{name, v, ok}
		}(name, command)
	}
	timeout := time.After(deadline)
	for range custom {
		select {
		case r := <-ch:
			if r.ok {
				out[r.name] = r.v
			}
		case <-timeout:
			// Whatever has not answered by now is dropped for THIS cycle. A
			// missing metric is a gap in a graph; a late push is a false page.
			return out
		}
	}
	return out
}

// gather runs the platform collectors, every [custom] command, and every
// [logwatch] scan. The second return value is the logwatch text samples.
//
// budget bounds the custom-command phase — see runCustomAll.
func gather(cfg *config, budget time.Duration) (map[string]any, map[string]string, []probeResult) {
	metrics := collect() // platform-specific (collect_linux.go / collect_windows.go)
	for name, v := range runCustomAll(cfg.Custom, budget) {
		metrics[name] = v
	}
	for name, v := range collectSNMP(cfg.SNMP) {
		metrics[name] = v
	}
	counts, samples := scanLogwatches(cfg.Logwatch)
	for name, n := range counts {
		metrics[name] = n
	}
	// Proxmox: the guests, storage and quorum of the hypervisor this agent runs
	// on. Merged last, and its sample (which guests are stopped) is added
	// alongside any [logwatch] samples rather than replacing them.
	if pm, psamples := collectProxmox(cfg.Proxmox); pm != nil {
		for name, v := range pm {
			metrics[name] = v
		}
		for name, s := range psamples {
			if samples == nil {
				samples = map[string]string{}
			}
			samples[name] = s
		}
	}
	return metrics, samples, runProbes(cfg.Probes, budget)
}

// banner prints the pylon-beacon mark once at startup when stdout is an
// interactive terminal — never into pipes, service journals, or --once runs.
// ANSI color only where the terminal advertises support; plain art otherwise.
func banner(cfg *config) {
	st, err := os.Stdout.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return
	}
	colorOK := os.Getenv("TERM") != "dumb"
	if runtime.GOOS == "windows" && os.Getenv("WT_SESSION") == "" && os.Getenv("TERM") == "" && os.Getenv("ANSICON") == "" {
		colorOK = false // classic conhost has no VT processing unless something enabled it
	}
	c := func(code, s string) string {
		if !colorOK {
			return s
		}
		return "\x1b[" + code + "m" + s + "\x1b[0m"
	}
	const peri, mint, blue, core, dim = "38;5;111", "38;5;49", "38;5;81", "38;5;122", "2"
	fmt.Println()
	fmt.Println("      " + c(peri, "_.-") + c(core, "o") + c(peri, "-._"))
	fmt.Println("    " + c(peri, ".'       '.") + "        " + c(core, "pylon-beacon") + " " + c(dim, version))
	fmt.Println("      " + c(mint, ".-\"\"-."))
	fmt.Println("     " + c(mint, "/  ") + c(blue, "~~") + c(mint, "  \\") + "          " + c(dim, "node  ") + cfg.Node)
	fmt.Println("     " + c(mint, "\\      /") + "          " + c(dim, "to    ") + cfg.URL)
	fmt.Println("      " + c(mint, "`-..-'") + "           " + c(dim, "every ") + strconv.Itoa(cfg.Interval) + "s")
	fmt.Println()
}

func main() {
	cfgPath := flag.String("config", defaultConfigPath(), "path to the config file")
	once := flag.Bool("once", false, "collect and push a single time, then exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("pylon-beacon", version)
		return
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil && !os.IsNotExist(err) {
		log.Fatalf("config %s: %v", *cfgPath, err)
	}
	cfg.ID = machineID(*cfgPath) // identity.go — from the OS, never the config
	// environment overrides (and the way to run with no config file at all)
	if v := os.Getenv("PYLON_KEY"); v != "" {
		cfg.Key = v
	}
	if v := os.Getenv("PYLON_URL"); v != "" {
		cfg.URL = strings.TrimRight(v, "/")
	}
	if v := os.Getenv("PYLON_NODE"); v != "" {
		cfg.Node = v
	}
	if v := os.Getenv("PYLON_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Interval = n
		}
	}
	if cfg.Key == "" {
		log.Fatalf("no API key: set `key = …` in %s or the PYLON_KEY environment variable (create an ingest-scoped key in PylonMon → Settings → Admin → Status page & API)", *cfgPath)
	}
	if cfg.Node == "" {
		if h, err := os.Hostname(); err == nil {
			cfg.Node = h
		} else {
			cfg.Node = "unnamed-node"
		}
	}
	if cfg.Interval < 15 {
		cfg.Interval = 15
	}

	if !*once {
		banner(cfg)
	}
	log.Printf("pylon-beacon %s: node %q -> %s every %ds (%d custom metric(s), %d log watch(es), %d log stream(s), %d probe(s))",
		version, cfg.Node, cfg.URL, cfg.Interval, len(cfg.Custom), len(cfg.Logwatch), len(cfg.Logs), len(cfg.Probes))

	// The loop is a fixed-period TICKER, not sleep-after-work.
	//
	// It used to end each pass with time.Sleep(interval), which makes the
	// interval the GAP BETWEEN cycles rather than the period. The real period
	// was interval + however long that cycle happened to take, so any node
	// whose collection is slow drifts silently past the silence window the
	// server is measuring it against.
	//
	// Measured on a production node, 2026-07-28 — a beacon configured
	// interval = 20 against a 30s server-side SLA, 444 pushes over three hours:
	//
	//	min 21s   p50 24s   p90 27s   p99 30s   max 31s   mean 24.3s
	//
	// It never once pushed at 20s. Its ~4s cycle had quietly spent 4.3s of the
	// 10s of headroom the configuration promised, so an ordinary tail spike
	// crossed the line and the node was paged as silent while it was healthy
	// and pushing. A second node with a 0.2s cycle sat at a mean of 20.4s and
	// was never paged — same build, same config, different collection cost.
	//
	// A ticker makes the period the period: collection time comes OUT of the
	// wait instead of being added to it. Go drops missed ticks rather than
	// queueing them, which is the behaviour we want — a cycle that overruns
	// costs one skipped push, never a catch-up burst.
	//
	// The custom-command phase is separately bounded (runCustomAll) so that a
	// cycle cannot outlast the interval and reintroduce the drift.
	period := time.Duration(cfg.Interval) * time.Second
	tick := time.NewTicker(period)
	defer tick.Stop()
	pushLoop(cfg, period, tick.C, *once, nil)
}

// pushLoop is the beacon's main loop, kept out of main() so the cadence is
// testable. tick supplies the period; stop, when non-nil, ends the loop.
//
// COLLECTION HAPPENS DURING THE WAIT, AND THE PUSH GOES OUT ON THE TICK.
//
// Ticking alone fixes the average but not the spread. With gather-then-push,
// consecutive check-ins land period + (this cycle's cost - the previous
// cycle's cost) apart, so a node whose collection cost VARIES still jitters
// either side of its interval even though it averages exactly right. Measured
// on a node with slow, variable collection after the loop became a ticker, 57
// check-ins: mean 20.1s — correct — but p90 25s and max 28s against a 30s
// silence window. Two seconds of margin is not a fix, it is a smaller version
// of the same bug.
//
// Gathering before the wait and pushing the moment the tick arrives removes
// collection from the equation entirely: check-ins land one period apart plus
// only the variance of the HTTP request itself. Same node, same commands.
//
// The cost is that metrics are up to one period old when they land. At any
// interval you would actually run, that is invisible — the vitals were a point
// sample of one instant regardless, and the graph's resolution is the interval.
// A late check-in, by contrast, is a false page.
//
// The FIRST check-in still goes out immediately rather than waiting a full
// period. That matters on restart: the agent has usually just been silent for
// some fraction of its window already, and adding a whole period before
// speaking up would page the very customer this change exists to protect.
func pushLoop(cfg *config, period time.Duration, tick <-chan time.Time, once bool, stop <-chan struct{}) {
	// Announce ourselves straight away — see above.
	metrics, samples, probes := gather(cfg, period/2)
	sendCheckIn(cfg, period, metrics, samples, probes)
	shipLogs(cfg, logshipClient)
	if once {
		return
	}

	for {
		// Collect for the NEXT check-in while we are waiting for it, so that
		// however long this takes, it is spent inside the period rather than
		// added to it.
		metrics, samples, probes = gather(cfg, period/2)
		select {
		case <-tick:
		case <-stop:
			return
		}
		sendCheckIn(cfg, period, metrics, samples, probes)
		// logs ride the same tick, after the check-in — a slow log post must
		// never make the node look silent
		shipLogs(cfg, logshipClient)
	}
}
