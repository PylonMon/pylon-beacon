package main

// netrates — turn interface byte counters into bytes-per-second (T-304).
//
// Every OS hands us cumulative counters; the customer wants activity. The
// rate is the delta since the previous gather divided by the elapsed time,
// so the first push omits it (like cpu_pct) and a counter that went backwards
// (interface reset) is skipped for one cycle rather than reported as a
// negative or absurd number.
//
// Reported as:
//
//	net_rx_bps / net_tx_bps   totals over real interfaces
//	net_if_rx_bps / net_if_tx_bps   per interface, only the ones with traffic
//	                                (a map, like disk_pct — the server renders
//	                                "net_if_rx_bps eth0")

import "time"

var (
	prevNet   map[string][2]uint64
	prevNetAt time.Time
)

func netRates(m map[string]any, cur map[string][2]uint64, now time.Time) {
	defer func() { prevNet, prevNetAt = cur, now }()
	if prevNet == nil || now.Sub(prevNetAt) < time.Second {
		return
	}
	dt := now.Sub(prevNetAt).Seconds()
	var rxT, txT float64
	rxIf, txIf := map[string]any{}, map[string]any{}
	for name, c := range cur {
		p, ok := prevNet[name]
		if !ok || c[0] < p[0] || c[1] < p[1] {
			continue // new interface, or counters reset — no rate this cycle
		}
		rx := float64(c[0]-p[0]) / dt
		tx := float64(c[1]-p[1]) / dt
		rxT += rx
		txT += tx
		if rx+tx > 0 && len(rxIf) < 8 {
			rxIf[name] = round1(rx)
			txIf[name] = round1(tx)
		}
	}
	m["net_rx_bps"] = round1(rxT)
	m["net_tx_bps"] = round1(txT)
	if len(rxIf) > 0 {
		m["net_if_rx_bps"] = rxIf
		m["net_if_tx_bps"] = txIf
	}
}
