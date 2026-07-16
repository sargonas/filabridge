package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*WebServer, *fakePrusaLink, *fakeSpoolman) {
	t.Helper()
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	bridge := newTestBridge(t, printer, spoolman)
	return NewWebServer(bridge), printer, spoolman
}

func doJSON(t *testing.T, ws *WebServer, method, path, body string) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	ws.router.ServeHTTP(rec, req)

	var parsed map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &parsed)
	return rec, parsed
}

func TestHealthz(t *testing.T) {
	ws, _, _ := newTestServer(t)
	rec, body := doJSON(t, ws, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d", rec.Code)
	}
	if body["status"] != "ok" {
		t.Errorf("healthz body: %v", body)
	}
}

// TestAutoAssignToggleAcceptsFalse guards against the gin binding:"required"
// regression: a bool field marked required rejects false as "missing", making
// the feature impossible to disable. This bug shipped once already.
func TestAutoAssignToggleAcceptsFalse(t *testing.T) {
	ws, _, _ := newTestServer(t)

	rec, _ := doJSON(t, ws, http.MethodPut, "/api/config/auto-assign-previous-spool", `{"enabled":true,"location":"Bin"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}

	rec, _ = doJSON(t, ws, http.MethodPut, "/api/config/auto-assign-previous-spool", `{"enabled":false,"location":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disabling must be possible: %d %s", rec.Code, rec.Body.String())
	}

	rec, body := doJSON(t, ws, http.MethodGet, "/api/config/auto-assign-previous-spool", "")
	if rec.Code != http.StatusOK || body["enabled"] != false {
		t.Errorf("after disable: %d %v", rec.Code, body)
	}
}

func TestPrintHistoryAPI(t *testing.T) {
	ws, _, _ := newTestServer(t)

	if err := ws.bridge.LogPrintUsage("P", 0, 1, 12.5, "job.gcode", time.Now(), "completed"); err != nil {
		t.Fatal(err)
	}

	rec, body := doJSON(t, ws, http.MethodGet, "/api/print-history", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("history = %d", rec.Code)
	}
	history, ok := body["history"].([]interface{})
	if !ok || len(history) != 1 {
		t.Fatalf("history body: %v", body)
	}
	row := history[0].(map[string]interface{})
	if row["status"] != "completed" || row["filament_used"] != 12.5 {
		t.Errorf("row: %v", row)
	}
}

func TestMapAndUnmapToolhead(t *testing.T) {
	ws, _, spoolman := newTestServer(t)
	spoolman.Spools[1] = &fakeSpool{ID: 1, Name: "Spool", RemainingWeight: 500}

	rec, _ := doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("map: %d %s", rec.Code, rec.Body.String())
	}
	if id, _ := ws.bridge.GetToolheadMapping("TestPrinter", 0); id != 1 {
		t.Fatalf("mapping not stored: %d", id)
	}

	// spool_id 0 unmaps
	rec, _ = doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unmap: %d %s", rec.Code, rec.Body.String())
	}
	if id, _ := ws.bridge.GetToolheadMapping("TestPrinter", 0); id != 0 {
		t.Fatalf("mapping not cleared: %d", id)
	}
}

// TestMapToolheadRejectsInvalidTargets: mappings to toolheads beyond the
// printer's configured count (or to unknown printers) must be rejected, not
// silently stored.
func TestMapToolheadRejectsInvalidTargets(t *testing.T) {
	ws, _, spoolman := newTestServer(t)
	spoolman.Spools[1] = &fakeSpool{ID: 1, Name: "Spool", RemainingWeight: 500}

	// TestPrinter has 1 toolhead, so toolhead_id 1 is out of range
	rec, _ := doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":1,"spool_id":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range toolhead must 400, got %d %s", rec.Code, rec.Body.String())
	}
	if id, _ := ws.bridge.GetToolheadMapping("TestPrinter", 1); id != 0 {
		t.Fatalf("out-of-range mapping was stored: %d", id)
	}

	rec, _ = doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"NoSuchPrinter","toolhead_id":0,"spool_id":1}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown printer must 404, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestSpoolConflictRejected(t *testing.T) {
	ws, _, spoolman := newTestServer(t)
	spoolman.Spools[1] = &fakeSpool{ID: 1, Name: "Spool", RemainingWeight: 500}

	ws.bridge.SavePrinterConfig("printer_two", PrinterConfig{Name: "Second", IPAddress: "127.0.0.1:1", APIKey: "k", Toolheads: 1})
	ws.bridge.ReloadConfig()

	doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":1}`)
	rec, _ := doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"Second","toolhead_id":0,"spool_id":1}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("same spool on two printers must 409, got %d", rec.Code)
	}
}

// TestDashboardSpoolmanLink: the tab bar links out to the configured Spoolman
// instance, and hides the link when no URL is configured.
func TestDashboardSpoolmanLink(t *testing.T) {
	ws, _, spoolman := newTestServer(t)

	rec, _ := doJSON(t, ws, http.MethodGet, "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `href="`+spoolman.URL()+`"`) {
		t.Fatal("Spoolman link missing from tab bar")
	}

	// Unconfigured Spoolman: no dead link
	ws.bridge.SetConfigValue(ConfigKeySpoolmanURL, "")
	ws.bridge.ReloadConfig()
	rec, _ = doJSON(t, ws, http.MethodGet, "/", "")
	if strings.Contains(rec.Body.String(), "Spoolman ↗") {
		t.Fatal("Spoolman link rendered without a configured URL")
	}
}
