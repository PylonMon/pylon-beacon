package main

import (
	"compress/gzip"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// What a check-in looks like on the wire: HTTP/1.1 even when the server offers
// HTTP/2, gzipped, and small enough to leave in one packet with its headers.
func TestCheckInIsCompressedHTTP1(t *testing.T) {
	var proto, enc atomic.Value
	var wire, plain atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto.Store(r.Proto)
		enc.Store(r.Header.Get("Content-Encoding"))
		raw, _ := io.ReadAll(r.Body)
		wire.Store(int64(len(raw)))
		zr, err := gzip.NewReader(strings.NewReader(string(raw)))
		if err == nil {
			b, _ := io.ReadAll(zr)
			plain.Store(int64(len(b)))
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	srv.EnableHTTP2 = true // the server OFFERS h2; the agent must still choose 1.1
	srv.StartTLS()
	defer srv.Close()
	pushTransport.TLSClientConfig = &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	defer func() { pushTransport.TLSClientConfig = nil; pushTransport.CloseIdleConnections() }()
	gzipOK.Store(true)

	metrics := map[string]any{"cpu_pct": 29.0, "mem_pct": 99.0, "load1": 0.4}
	for i := 0; i < 40; i++ { // a realistic, chatty node
		metrics["disk_pct "+string(rune('C'+i%20))+":\\"] = 25.5
	}
	cfg := &config{Node: "DC1", URL: srv.URL, Key: "k", Interval: 30, ID: "machine-1"}
	if err := push(cfg, metrics, map[string]string{"secerr": strings.Repeat("Error line from the security log\n", 60)}, nil, 5*time.Second, false); err != nil {
		t.Fatal(err)
	}
	if p, _ := proto.Load().(string); p != "HTTP/1.1" {
		t.Fatalf("check-in went over %s — over HTTP/2 headers and body are separate frames and can arrive in halves", p)
	}
	if e, _ := enc.Load().(string); e != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", e)
	}
	t.Logf("check-in: %d bytes plain -> %d bytes on the wire", plain.Load(), wire.Load())
	if wire.Load() == 0 || wire.Load() > 1200 {
		t.Fatalf("compressed check-in is %d bytes — it should fit in one packet with its headers", wire.Load())
	}
}

// A server that predates compressed check-ins answers 400. The agent must
// deliver THAT check-in anyway (plain) and stop compressing — never go silent
// because of an optimisation.
func TestOldServerGetsAPlainCheckIn(t *testing.T) {
	var gz, plainHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz.Add(1)
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"bad json"}`))
			return
		}
		plainHits.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	defer pushTransport.CloseIdleConnections()
	gzipOK.Store(true)
	defer gzipOK.Store(true)
	cfg := &config{Node: "n", URL: srv.URL, Key: "k", Interval: 30}
	for i := 0; i < 3; i++ {
		if err := push(cfg, map[string]any{"cpu_pct": 1.0}, nil, nil, 5*time.Second, false); err != nil {
			t.Fatalf("check-in %d failed against an old server: %v", i, err)
		}
	}
	if gz.Load() != 1 || plainHits.Load() != 3 {
		t.Fatalf("gzip tried %d times (want 1), plain delivered %d (want 3)", gz.Load(), plainHits.Load())
	}
}
