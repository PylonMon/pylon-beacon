package main

import (
	"testing"
	"time"
)

// Counters in, bytes-per-second out: first gather is silent, the second
// reports the delta over the elapsed time, a reset interface is skipped for
// one cycle, loopback-free totals and a per-interface breakdown.
func TestNetRatesFromCounters(t *testing.T) {
	prevNet, prevNetAt = nil, time.Time{}
	t0 := time.Now()
	m := map[string]any{}
	netRates(m, map[string][2]uint64{"eth0": {1000, 500}, "wlan0": {10, 10}}, t0)
	if _, ok := m["net_rx_bps"]; ok {
		t.Fatal("first gather must not report a rate")
	}
	m = map[string]any{}
	netRates(m, map[string][2]uint64{"eth0": {1000 + 20000, 500 + 4000}, "wlan0": {10, 10}}, t0.Add(20*time.Second))
	if m["net_rx_bps"] != 1000.0 || m["net_tx_bps"] != 200.0 {
		t.Fatalf("rates: rx=%v tx=%v (want 1000 / 200 B/s)", m["net_rx_bps"], m["net_tx_bps"])
	}
	rx := m["net_if_rx_bps"].(map[string]any)
	if rx["eth0"] != 1000.0 {
		t.Fatalf("per-interface: %v", rx)
	}
	if _, ok := rx["wlan0"]; ok {
		t.Fatal("an idle interface should not appear in the breakdown")
	}
	// counter reset (interface re-created): skipped this cycle, no negative rate
	m = map[string]any{}
	netRates(m, map[string][2]uint64{"eth0": {5, 5}, "wlan0": {10, 10}}, t0.Add(40*time.Second))
	if m["net_rx_bps"] != 0.0 {
		t.Fatalf("a reset counter produced a rate: %v", m["net_rx_bps"])
	}
}
