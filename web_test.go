package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
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

// TestDeveloperModeFlag: the dashboard exposes FILABRIDGE_DEVELOPER_MODE to the
// frontend via a body data-attribute, off by default and on when the env var is
// set. This is the gate the (in-development) Bambu support hides behind.
func TestDeveloperModeFlag(t *testing.T) {
	t.Run("on", func(t *testing.T) {
		t.Setenv("FILABRIDGE_DEVELOPER_MODE", "true")
		ws, _, _ := newTestServer(t)
		rec, _ := doJSON(t, ws, http.MethodGet, "/", "")
		if !strings.Contains(rec.Body.String(), `data-developer-mode="true"`) {
			t.Error("expected developer mode exposed as true when env var is set")
		}
	})
	t.Run("off", func(t *testing.T) {
		t.Setenv("FILABRIDGE_DEVELOPER_MODE", "")
		ws, _, _ := newTestServer(t)
		rec, _ := doJSON(t, ws, http.MethodGet, "/", "")
		if !strings.Contains(rec.Body.String(), `data-developer-mode="false"`) {
			t.Error("expected developer mode off by default")
		}
	})
}

// TestPrinterTypePersists: a printer's type and serial round-trip through the
// schema, and a printer saved without a type reads back as PrusaLink.
func TestPrinterTypePersists(t *testing.T) {
	ws, _, _ := newTestServer(t) // registers "printer_test" (no Type set)

	if err := ws.bridge.SavePrinterConfig("printer_bambu", PrinterConfig{
		Name: "X1C", IPAddress: "192.168.1.9", APIKey: "accesscode", Toolheads: 4,
		Type: PrinterTypeBambu, Serial: "01S00A1234567890",
	}); err != nil {
		t.Fatal(err)
	}

	configs, err := ws.bridge.GetAllPrinterConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if got := configs["printer_bambu"]; got.Type != PrinterTypeBambu || got.Serial != "01S00A1234567890" {
		t.Errorf("bambu type/serial not persisted: %+v", got)
	}
	if got := configs["printer_test"]; got.Type != PrinterTypePrusaLink {
		t.Errorf("default printer type = %q, want %q", got.Type, PrinterTypePrusaLink)
	}
}

// TestAddPrinterBambuGating: Bambu printers are rejected unless developer mode
// is on, require a serial number, and are accepted once both hold.
func TestAddPrinterBambuGating(t *testing.T) {
	// Developer mode OFF (default): a Bambu printer is forbidden.
	ws, _, _ := newTestServer(t)
	rec, _ := doJSON(t, ws, http.MethodPost, "/api/printers",
		`{"name":"MyA1","ip_address":"192.168.1.9","api_key":"code","toolheads":4,"type":"bambu","serial":"01S00A"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bambu without developer mode must 403, got %d %s", rec.Code, rec.Body.String())
	}

	// Developer mode ON.
	t.Setenv("FILABRIDGE_DEVELOPER_MODE", "true")
	ws2, _, _ := newTestServer(t)

	// A Bambu printer with no serial is a bad request.
	rec, _ = doJSON(t, ws2, http.MethodPost, "/api/printers",
		`{"name":"MyA1","ip_address":"192.168.1.9","api_key":"code","toolheads":4,"type":"bambu"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bambu without serial must 400, got %d %s", rec.Code, rec.Body.String())
	}

	// With developer mode and a serial, it is accepted and persists as bambu.
	rec, body := doJSON(t, ws2, http.MethodPost, "/api/printers",
		`{"name":"MyA1","ip_address":"192.168.1.9","api_key":"code","toolheads":4,"type":"bambu","serial":"01S00A1234567890"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("bambu with developer mode + serial must 200, got %d %s", rec.Code, rec.Body.String())
	}
	id, _ := body["printer_id"].(string)
	configs, err := ws2.bridge.GetAllPrinterConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if got := configs[id]; got.Type != PrinterTypeBambu || got.Serial != "01S00A1234567890" {
		t.Errorf("bambu printer not persisted correctly: %+v", got)
	}
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

// TestToolheadMappingSyncsSpoolmanLocation: assigning a spool to a toolhead
// (via any path) sets its Spoolman location to "PrinterName - ToolheadName";
// the spool it displaces, and any spool that is later unmapped, is relocated to
// the configured storage location, or has its location cleared when none is set.
func TestToolheadMappingSyncsSpoolmanLocation(t *testing.T) {
	ws, _, spoolman := newTestServer(t)
	spoolman.Spools[1] = &fakeSpool{ID: 1, Name: "Red", RemainingWeight: 500}
	spoolman.Spools[2] = &fakeSpool{ID: 2, Name: "Blue", RemainingWeight: 500}

	// Assign spool 1 -> its location becomes the toolhead location
	rec, _ := doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("map spool 1: %d %s", rec.Code, rec.Body.String())
	}
	if got := spoolman.Spools[1].Location; got != "TestPrinter - Toolhead 0" {
		t.Fatalf("spool 1 location = %q, want %q", got, "TestPrinter - Toolhead 0")
	}

	// Assign spool 2 to the same toolhead. Auto-assign is off, so the displaced
	// spool 1 has its location cleared rather than moved to storage.
	rec, _ = doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("map spool 2: %d %s", rec.Code, rec.Body.String())
	}
	if got := spoolman.Spools[2].Location; got != "TestPrinter - Toolhead 0" {
		t.Fatalf("spool 2 location = %q, want toolhead location", got)
	}
	if got := spoolman.Spools[1].Location; got != "" {
		t.Fatalf("displaced spool 1 location = %q, want cleared", got)
	}

	// With auto-assign enabled and the storage location present in Spoolman,
	// unmapping moves the spool there instead of clearing it.
	spoolman.Locations = []string{"Storage"}
	if err := ws.bridge.SetAutoAssignPreviousSpoolEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := ws.bridge.SetAutoAssignPreviousSpoolLocation("Storage"); err != nil {
		t.Fatal(err)
	}

	rec, _ = doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unmap: %d %s", rec.Code, rec.Body.String())
	}
	if got := spoolman.Spools[2].Location; got != "Storage" {
		t.Fatalf("unmapped spool 2 location = %q, want %q", got, "Storage")
	}
}

// TestRelocateToConfiguredEmptyLocation: unmapping moves the displaced spool to
// the configured auto-assign location even when that location currently holds no
// spools (so it is absent from Spoolman's /location list). Regression for the
// bug where an empty default location caused the spool's location to be cleared
// ("no location") instead of moved.
func TestRelocateToConfiguredEmptyLocation(t *testing.T) {
	ws, _, spoolman := newTestServer(t)
	spoolman.Spools[1] = &fakeSpool{ID: 1, Name: "Red", RemainingWeight: 500}

	// Auto-assign on, pointing at a location that holds no spools (empty), so it
	// is NOT present in spoolman.Locations / the /location list.
	if err := ws.bridge.SetAutoAssignPreviousSpoolEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := ws.bridge.SetAutoAssignPreviousSpoolLocation("Drybox"); err != nil {
		t.Fatal(err)
	}

	doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":1}`)
	rec, _ := doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unmap: %d %s", rec.Code, rec.Body.String())
	}

	if got := spoolman.Spools[1].Location; got != "Drybox" {
		t.Fatalf("spool location = %q, want %q (should move to the empty configured location, not clear)", got, "Drybox")
	}
}

// TestLocationsListEmptyAndTypeToolheads: /api/locations reads the predefined
// locations setting so empty locations (no spools) still appear, and this
// instance's toolhead locations are typed "printer" so the storage dropdown
// filters them out.
func TestLocationsListEmptyAndTypeToolheads(t *testing.T) {
	ws, _, spoolman := newTestServer(t)
	// "Unopened" holds no spools; "TestPrinter - Toolhead 0" is a toolhead location.
	spoolman.LocationSetting = []string{"Drybox", "Unopened", "TestPrinter - Toolhead 0"}

	rec, body := doJSON(t, ws, http.MethodGet, "/api/locations", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("locations: %d", rec.Code)
	}
	locs, _ := body["locations"].([]interface{})
	types := map[string]string{}
	for _, l := range locs {
		m := l.(map[string]interface{})
		types[m["name"].(string)] = m["type"].(string)
	}
	if types["Unopened"] != "storage" {
		t.Errorf("empty location 'Unopened' should be listed as storage; got %q (types: %v)", types["Unopened"], types)
	}
	if types["Drybox"] != "storage" {
		t.Errorf("Drybox type = %q, want storage", types["Drybox"])
	}
	if types["TestPrinter - Toolhead 0"] != "printer" {
		t.Errorf("toolhead location should be typed 'printer'; got %q", types["TestPrinter - Toolhead 0"])
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

// TestAddPrinterRejectsDuplicateName: printer names must be unique so that
// toolhead-location strings ("Name - Toolhead") stay unambiguous. Adding a
// second printer with an existing name is rejected; renaming a printer onto
// another's name is rejected; keeping a printer's own name on update is allowed.
func TestAddPrinterRejectsDuplicateName(t *testing.T) {
	ws, _, _ := newTestServer(t) // newTestBridge already registered "TestPrinter"

	// Adding another printer with the same name is a conflict.
	rec, _ := doJSON(t, ws, http.MethodPost, "/api/printers",
		`{"name":"TestPrinter","ip_address":"127.0.0.1:9","api_key":"k","toolheads":1}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate add must 409, got %d %s", rec.Code, rec.Body.String())
	}

	// A distinct name is accepted.
	rec, body := doJSON(t, ws, http.MethodPost, "/api/printers",
		`{"name":"SecondPrinter","ip_address":"127.0.0.1:9","api_key":"k","toolheads":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("distinct add must 200, got %d %s", rec.Code, rec.Body.String())
	}
	secondID, _ := body["printer_id"].(string)
	if secondID == "" {
		t.Fatalf("no printer_id returned: %v", body)
	}

	// Renaming SecondPrinter onto the existing "TestPrinter" is a conflict.
	rec, _ = doJSON(t, ws, http.MethodPut, "/api/printers/"+secondID,
		`{"name":"TestPrinter","ip_address":"127.0.0.1:9","api_key":"k","toolheads":1}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("rename onto existing name must 409, got %d %s", rec.Code, rec.Body.String())
	}

	// Updating SecondPrinter while keeping its own name is allowed.
	rec, _ = doJSON(t, ws, http.MethodPut, "/api/printers/"+secondID,
		`{"name":"SecondPrinter","ip_address":"127.0.0.1:10","api_key":"k","toolheads":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("self-name update must 200, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestTabButtonsHaveMatchingContent guards the contract the tab-persistence JS
// relies on: every main tab button carries a data-tab attribute whose value has
// a matching content div (id="<value>-tab"). If a data-tab is dropped or renamed
// in the template, restoreActiveTab silently stops restoring that tab on reload;
// this render test catches it. (The Spoolman entry is an external link with no
// switchTab call, so it is intentionally excluded.)
func TestTabButtonsHaveMatchingContent(t *testing.T) {
	ws, _, _ := newTestServer(t)
	rec, _ := doJSON(t, ws, http.MethodGet, "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: %d", rec.Code)
	}
	body := rec.Body.String()

	matches := regexp.MustCompile(`switchTab\('([^']+)'\)`).FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		t.Fatal("no tab buttons found in rendered page")
	}

	seen := map[string]bool{}
	for _, m := range matches {
		tab := m[1]
		if seen[tab] {
			continue
		}
		seen[tab] = true
		if !strings.Contains(body, `data-tab="`+tab+`"`) {
			t.Errorf("tab %q button is missing its data-tab attribute", tab)
		}
		if !strings.Contains(body, `id="`+tab+`-tab"`) {
			t.Errorf("tab %q has no matching content div id=%q", tab, tab+"-tab")
		}
	}

	// The always-present tabs must be among those rendered.
	for _, tab := range []string{"status", "nfc", "settings"} {
		if !seen[tab] {
			t.Errorf("expected tab %q not rendered", tab)
		}
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
