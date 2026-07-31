package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNormalizeProgress(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{0, 0},
		{1.0, 0.01}, // 1% - the historical 100x overbilling bug
		{40, 0.4},
		{100, 1.0},
		{150, 1.0}, // clamped
		{-5, 0},    // clamped
	}
	for _, c := range cases {
		if got := normalizeProgress(c.in); got != c.want {
			t.Errorf("normalizeProgress(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseGcodeFilamentUsage(t *testing.T) {
	client := &PrusaLinkClient{}

	cases := []struct {
		name string
		in   string
		want map[int]float64
	}{
		{"bgcode style", "junk|filament used [g]=372.68|more", map[int]float64{0: 372.68}},
		{"ascii style", "; filament used [g] = 1.23, 4.56\n", map[int]float64{0: 1.23, 1: 4.56}},
		{"multi toolhead", "filament used [g]=1.0,0,3.5", map[int]float64{0: 1.0, 2: 3.5}}, // zero weights skipped
		{"absent", "no metadata here", map[int]float64{}},
	}
	for _, c := range cases {
		got, err := client.ParseGcodeFilamentUsage([]byte(c.in))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: toolhead %d = %v, want %v", c.name, k, got[k], v)
			}
		}
	}
}

func TestFilamentUsageFromMeta(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]interface{}
		want map[int]float64
	}{
		{"nil meta", nil, map[int]float64{}},
		{"string value", map[string]interface{}{"filament used [g]": "12.5,3.0"}, map[int]float64{0: 12.5, 1: 3.0}},
		{"float value", map[string]interface{}{"filament used [g]": 9.75}, map[int]float64{0: 9.75}},
		{"array value", map[string]interface{}{"filament used [g]": []interface{}{1.5, "2.5"}}, map[int]float64{0: 1.5, 1: 2.5}},
		{"missing key", map[string]interface{}{"other": "x"}, map[int]float64{}},
	}
	for _, c := range cases {
		got := filamentUsageFromMeta(c.meta)
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: toolhead %d = %v, want %v", c.name, k, got[k], v)
			}
		}
	}
}

// scanServer serves body for any path, optionally never terminating (to prove
// the scanner hangs up early instead of reading to the end).
func scanServer(t *testing.T, body []byte, endless bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
		if endless {
			flusher := w.(http.Flusher)
			flusher.Flush()
			junk := bytes.Repeat([]byte{0}, 64<<10)
			for i := 0; i < 100000; i++ {
				if _, err := w.Write(junk); err != nil {
					return // scanner hung up - expected
				}
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func scanClient(srv *httptest.Server) *PrusaLinkClient {
	return NewPrusaLinkClient(strings.TrimPrefix(srv.URL, "http://"), "key", 5, 30)
}

func TestScanEarlyExitOnHugeFile(t *testing.T) {
	srv := scanServer(t, bgcodeFixture("372.68", 0), true) // endless stream after metadata
	client := scanClient(srv)

	start := time.Now()
	usage, err := client.ScanGcodeFilamentUsage("usb/huge.bgcode", 30)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if usage[0] != 372.68 {
		t.Fatalf("got %v, want 372.68", usage)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("scanner did not exit early: took %v", elapsed)
	}
}

func TestScanValuesAtEndOfFile(t *testing.T) {
	// ASCII .gcode style: values on the very last line
	body := append(bytes.Repeat([]byte("; filler comment line\n"), 20000),
		[]byte("; filament used [g] = 5.25")...)
	srv := scanServer(t, body, false)

	usage, err := scanClient(srv).ScanGcodeFilamentUsage("usb/small.gcode", 30)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if usage[0] != 5.25 {
		t.Fatalf("got %v, want 5.25", usage)
	}
}

func TestScanChunkBoundarySafety(t *testing.T) {
	// Place the value so it straddles the scanner's 64KB chunk boundary; a
	// naive scanner would parse the truncated "372.6" from the first chunk.
	head := bytes.Repeat([]byte{'x'}, (64<<10)-20)
	body := append(head, []byte("filament used [g]=372.68|trailing data beyond the boundary to settle the match")...)
	srv := scanServer(t, body, false)

	usage, err := scanClient(srv).ScanGcodeFilamentUsage("usb/boundary.bgcode", 30)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if usage[0] != 372.68 {
		t.Fatalf("got %v, want exactly 372.68 (boundary truncation?)", usage)
	}
}

func TestScanGivesUpAtCap(t *testing.T) {
	// No metadata anywhere; more data than the cap. Scanner must return empty
	// (not error, not hang) so the caller can fall back to a full download.
	srv := scanServer(t, bytes.Repeat([]byte{'y'}, 1<<20), true)

	usage, err := scanClient(srv).ScanGcodeFilamentUsage("usb/nometa.bgcode", 30)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(usage) != 0 {
		t.Fatalf("expected empty usage, got %v", usage)
	}
}

func TestScanHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	t.Cleanup(srv.Close)

	if _, err := scanClient(srv).ScanGcodeFilamentUsage("usb/blocked.bgcode", 5); err == nil {
		t.Fatal("expected error for 409 response")
	}
}

// bareBridge creates a bridge with no printers or fakes wired up, for tests
// that exercise bridge state directly rather than a monitor cycle.
func bareBridge(t *testing.T) *FilamentBridge {
	t.Helper()
	t.Setenv("FILABRIDGE_DB_PATH", t.TempDir())

	bridge, err := NewFilamentBridge(nil)
	if err != nil {
		t.Fatalf("NewFilamentBridge: %v", err)
	}
	t.Cleanup(func() { bridge.Close() })
	return bridge
}

// countingServer reports how many distinct TCP connections it has accepted.
func countingServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	conns := make(map[net.Conn]bool)

	srv := httptest.NewUnstartedServer(handler)
	srv.Config.ConnState = func(c net.Conn, state http.ConnState) {
		if state == http.StateNew {
			mu.Lock()
			conns[c] = true
			mu.Unlock()
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(conns)
	}
}

// TestClientReusesConnection is the regression guard for issue #35: PrusaLink
// runs its HTTP server on a wifi coprocessor with a very small socket pool, and
// a client that opens a fresh connection per request can exhaust it. A reused
// client must poll over a single connection no matter how many requests it makes.
func TestClientReusesConnection(t *testing.T) {
	srv, connCount := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"printer":{"state":"IDLE"}}`))
	})

	client := NewPrusaLinkClient(strings.TrimPrefix(srv.URL, "http://"), "key", 5, 30)
	for i := 0; i < 20; i++ {
		if _, err := client.GetStatus(); err != nil {
			t.Fatalf("status %d: %v", i, err)
		}
	}

	if got := connCount(); got != 1 {
		t.Fatalf("20 polls opened %d connections, want 1", got)
	}
}

// TestBridgeReusesClientAcrossCallers pins the bridge-level half of the fix: the
// monitor loop and the dashboard/broadcast read path must share one pooled
// client per printer rather than each building their own transport.
func TestBridgeReusesClientAcrossCallers(t *testing.T) {
	b := bareBridge(t)
	cfg := &Config{PrusaLinkTimeout: 5, PrusaLinkFileDownloadTimeout: 30}
	pc := PrinterConfig{Name: "Core One", IPAddress: "192.0.2.10"}

	first := b.prusaClientFor("p1", pc, cfg)
	second := b.prusaClientFor("p1", pc, cfg)
	if first != second {
		t.Fatal("prusaClientFor returned a new client for unchanged settings")
	}

	// A changed address must produce a different client, not keep talking to
	// the old one.
	moved := b.prusaClientFor("p1", PrinterConfig{Name: "Core One", IPAddress: "192.0.2.11"}, cfg)
	if moved == first {
		t.Fatal("prusaClientFor reused the client after the address changed")
	}
}

// TestGetStatusServesFreshCache pins that a status read behind a recent monitor
// poll does not touch the printer at all.
func TestGetStatusServesFreshCache(t *testing.T) {
	b := bareBridge(t)
	b.cachePrinterStatus("p1", StatePrinting, nil)

	cached, fresh := b.cachedPrinterState("p1")
	if !fresh {
		t.Fatal("status cached just now reported as stale")
	}
	if cached.state != StatePrinting {
		t.Fatalf("got state %q, want %q", cached.state, StatePrinting)
	}

	// An entry older than the TTL must fall through to a live poll.
	b.statusCacheMutex.Lock()
	b.printerStatusCache["p1"] = cachedPrinterStatus{
		state:     StatePrinting,
		fetchedAt: time.Now().Add(-PrinterStatusTTL - time.Second),
	}
	b.statusCacheMutex.Unlock()

	if _, fresh := b.cachedPrinterState("p1"); fresh {
		t.Fatal("expired cache entry reported as fresh")
	}
}
