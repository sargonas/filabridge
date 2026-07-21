package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakePrusaLink is an in-process PrusaLink simulator. State is mutated by
// tests (and by pause/resume calls from the code under test) to walk a print
// through its lifecycle without any real printer or timing dependency.
type fakePrusaLink struct {
	mu         sync.Mutex
	State      string // IDLE, PRINTING, PAUSED, FINISHED, STOPPED, ATTENTION...
	JobID      int
	Progress   float64 // percent, 0-100, as the real API reports
	Filename   string  // e.g. "big.bgcode"
	Meta       map[string]interface{}
	FileBody   []byte // served for /usb/<Filename>
	BlockFiles bool   // 409 on file reads (printer busy)

	PauseCalls  int
	ResumeCalls int

	srv *httptest.Server
}

func newFakePrusaLink(t *testing.T) *fakePrusaLink {
	t.Helper()
	f := &fakePrusaLink{State: "IDLE"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// Addr returns host:port suitable for PrinterConfig.IPAddress
func (f *fakePrusaLink) Addr() string {
	return strings.TrimPrefix(f.srv.URL, "http://")
}

func (f *fakePrusaLink) set(fn func(*fakePrusaLink)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakePrusaLink) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.URL.Path == "/api/v1/status":
		writeJSON(w, map[string]interface{}{"printer": map[string]interface{}{"state": f.State}})

	case r.URL.Path == "/api/v1/job" && r.Method == http.MethodGet:
		if f.State != "PRINTING" && f.State != "PAUSED" && f.State != "ATTENTION" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, map[string]interface{}{
			"id":       f.JobID,
			"state":    f.State,
			"progress": f.Progress,
			"file": map[string]interface{}{
				"name":         f.Filename,
				"display_name": f.Filename,
				"path":         "/usb",
				"refs":         map[string]interface{}{"download": "/usb/" + f.Filename},
				"meta":         f.Meta,
			},
		})

	case strings.HasPrefix(r.URL.Path, "/api/v1/job/") && strings.HasSuffix(r.URL.Path, "/pause") && r.Method == http.MethodPut:
		f.PauseCalls++
		if f.State == "PRINTING" {
			f.State = "PAUSED"
		}
		w.WriteHeader(http.StatusNoContent)

	case strings.HasPrefix(r.URL.Path, "/api/v1/job/") && strings.HasSuffix(r.URL.Path, "/resume") && r.Method == http.MethodPut:
		f.ResumeCalls++
		if f.State == "PAUSED" {
			f.State = "PRINTING"
		}
		w.WriteHeader(http.StatusNoContent)

	case strings.HasPrefix(r.URL.Path, "/usb/"):
		if f.BlockFiles {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"printer is busy"}`)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(f.FileBody)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// fakeSpoolman is an in-process Spoolman simulator with just enough surface
// for the bridge: spool lookup/list and the used_weight PATCH.
type fakeSpoolman struct {
	mu     sync.Mutex
	Spools map[int]*fakeSpool

	PatchCalls      []int    // spool IDs that received usage updates
	Locations       []string // locations returned by /api/v1/location (only ones with spools)
	LocationSetting []string // predefined locations setting; nil => endpoint 404s (fall back to /location)

	srv *httptest.Server
}

type fakeSpool struct {
	ID              int
	Name            string
	RemainingWeight float64
	UsedWeight      float64
	Archived        bool
	Location        string
}

func newFakeSpoolman(t *testing.T) *fakeSpoolman {
	t.Helper()
	f := &fakeSpoolman{Spools: map[int]*fakeSpool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSpoolman) URL() string { return f.srv.URL }

func (f *fakeSpoolman) spoolJSON(s *fakeSpool) map[string]interface{} {
	return map[string]interface{}{
		"id":               s.ID,
		"remaining_weight": s.RemainingWeight,
		"used_weight":      s.UsedWeight,
		"archived":         s.Archived,
		"location":         s.Location,
		"filament": map[string]interface{}{
			"id": s.ID, "name": s.Name, "material": "PLA",
			"vendor": map[string]interface{}{"id": 1, "name": "TestVendor"},
		},
	}
}

func (f *fakeSpoolman) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.URL.Path == "/api/v1/spool" && r.Method == http.MethodGet:
		var list []map[string]interface{}
		for _, s := range f.Spools {
			list = append(list, f.spoolJSON(s))
		}
		writeJSON(w, list)

	case strings.HasPrefix(r.URL.Path, "/api/v1/spool/") && r.Method == http.MethodGet:
		id := spoolIDFromPath(r.URL.Path)
		if s, ok := f.Spools[id]; ok {
			writeJSON(w, f.spoolJSON(s))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"detail":"no such spool"}`)

	case strings.HasPrefix(r.URL.Path, "/api/v1/spool/") && r.Method == http.MethodPatch:
		id := spoolIDFromPath(r.URL.Path)
		s, ok := f.Spools[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		if v, ok := body["used_weight"].(float64); ok {
			delta := v - s.UsedWeight
			s.UsedWeight = v
			s.RemainingWeight -= delta
			f.PatchCalls = append(f.PatchCalls, id)
		}
		if v, ok := body["location"].(string); ok {
			s.Location = v
		}
		writeJSON(w, f.spoolJSON(s))

	case r.URL.Path == "/api/v1/filament":
		writeJSON(w, []interface{}{})

	case r.URL.Path == "/api/v1/setting/locations":
		if f.LocationSetting == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		value, _ := json.Marshal(f.LocationSetting)
		writeJSON(w, map[string]interface{}{"value": string(value), "is_set": true, "type": "array"})

	case r.URL.Path == "/api/v1/location":
		locs := make([]map[string]interface{}, 0, len(f.Locations))
		for i, name := range f.Locations {
			locs = append(locs, map[string]interface{}{"id": i + 1, "name": name})
		}
		writeJSON(w, locs)

	case r.URL.Path == "/api/v1/info":
		writeJSON(w, map[string]interface{}{"version": "fake"})

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func spoolIDFromPath(path string) int {
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	var id int
	fmt.Sscanf(parts[len(parts)-1], "%d", &id)
	return id
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// bgcodeFixture builds a synthetic .bgcode-style byte stream: binary junk with
// the slicer metadata line embedded at approximately the offset real files use
// (a few KB in), followed by tailJunk more bytes of noise.
func bgcodeFixture(weights string, tailJunk int) []byte {
	head := make([]byte, 4096)
	for i := range head {
		head[i] = byte(i % 251)
	}
	meta := []byte("printer_model=COREONE|filament used [mm]=127001.77|filament used [g]=" + weights + "|estimated printing time=1d16h49m|")
	tail := make([]byte, tailJunk)
	for i := range tail {
		tail[i] = byte((i * 7) % 253)
	}
	return append(append(head, meta...), tail...)
}

// newTestBridge creates a bridge backed by a temp-dir database, wired to the
// given fakes, with one printer and a fast test configuration.
func newTestBridge(t *testing.T, printer *fakePrusaLink, spoolman *fakeSpoolman) *FilamentBridge {
	t.Helper()
	t.Setenv("FILABRIDGE_DB_PATH", t.TempDir())

	bridge, err := NewFilamentBridge(nil)
	if err != nil {
		t.Fatalf("NewFilamentBridge: %v", err)
	}
	t.Cleanup(func() { bridge.Close() })

	if err := bridge.SavePrinterConfig("printer_test", PrinterConfig{
		Name:      "TestPrinter",
		IPAddress: printer.Addr(),
		APIKey:    "test-key",
		Toolheads: 1,
	}); err != nil {
		t.Fatalf("SavePrinterConfig: %v", err)
	}
	if err := bridge.SetConfigValue(ConfigKeySpoolmanURL, spoolman.URL()); err != nil {
		t.Fatalf("SetConfigValue: %v", err)
	}

	config, err := LoadConfig(bridge)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := bridge.UpdateConfig(config); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	return bridge
}

// cycle runs one synchronous monitor pass for the test printer, replacing the
// timing-dependent MonitorPrinters goroutine fan-out.
func cycle(t *testing.T, b *FilamentBridge) {
	t.Helper()
	cfg := b.GetConfigSnapshot()
	pc, ok := cfg.Printers["printer_test"]
	if !ok {
		t.Fatal("test printer missing from config")
	}
	if err := b.monitorPrusaLink("printer_test", pc); err != nil {
		t.Fatalf("monitorPrusaLink: %v", err)
	}
}
