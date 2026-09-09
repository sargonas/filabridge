package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*WebServer, *fakePrusaLink, *fakeSpoolman) {
	return newTestServerAtBasePath(t, "")
}

func newTestServerAtBasePath(t *testing.T, basePath string) (*WebServer, *fakePrusaLink, *fakeSpoolman) {
	t.Helper()
	t.Setenv("FILABRIDGE_BASE_PATH", basePath)
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

func TestConfiguredBasePath(t *testing.T) {
	tests := map[string]struct {
		value string
		want  string
	}{
		"unset":           {want: ""},
		"root":            {value: "/", want: ""},
		"absolute path":   {value: "/filabridge", want: "/filabridge"},
		"relative path":   {value: "filabridge", want: "/filabridge"},
		"trailing slash":  {value: "/filabridge/", want: "/filabridge"},
		"normalizes path": {value: "/proxy/./apps//filabridge/", want: "/proxy/apps/filabridge"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("FILABRIDGE_BASE_PATH", tc.value)
			if got := configuredBasePath(); got != tc.want {
				t.Errorf("configuredBasePath() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConfiguredBasePathRoutesAndRendersAllPages(t *testing.T) {
	basePath := "/api/hassio_ingress/test-token/"
	ws, _, _ := newTestServerAtBasePath(t, strings.TrimSuffix(basePath, "/"))

	pages := map[string]struct {
		path  string
		links []string
	}{
		"dashboard": {
			path: basePath,
			links: []string{
				`<link rel="stylesheet" href="` + basePath + `static/css/main.css">`,
				`<script src="` + basePath + `static/js/websocket.js"></script>`,
			},
		},
		"standalone": {
			path: basePath + "api/nfc/assign",
			links: []string{
				`<link rel="stylesheet" href="` + basePath + `static/css/v2/tokens.css">`,
				`<a href="` + basePath + `" class="back-button">Back to Dashboard</a>`,
			},
		},
	}

	for name, page := range pages {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, page.path, nil)
			rec := httptest.NewRecorder()
			ws.router.ServeHTTP(rec, req)
			body := rec.Body.String()

			if !strings.Contains(body, `<base href="`+basePath+`">`) {
				t.Fatalf("page did not render configured base path: %s", body)
			}
			for _, link := range page.links {
				if !strings.Contains(body, link) {
					t.Errorf("page missing prefixed link %q", link)
				}
			}
		})
	}

	for _, requestPath := range []string{"/", "/api/status", "/static/css/main.css"} {
		req := httptest.NewRequest(http.MethodGet, requestPath, nil)
		rec := httptest.NewRecorder()
		ws.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("unprefixed %s returned %d, want 404", requestPath, rec.Code)
		}
	}

	for _, requestPath := range []string{basePath + "api/status", basePath + "static/css/main.css"} {
		req := httptest.NewRequest(http.MethodGet, requestPath, nil)
		rec := httptest.NewRecorder()
		ws.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("prefixed %s returned %d, want 200", requestPath, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	ws.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("root healthcheck returned %d, want 200", rec.Code)
	}
}

func TestFrontendRequestsUseDocumentBase(t *testing.T) {
	rootFetch := regexp.MustCompile(`fetch\(\s*['\x60"]\/api\/`)
	for _, name := range []string{"main.js", "dropdowns.js", "printers.js", "websocket.js", "nfc.js"} {
		data, err := staticFS.ReadFile("static/js/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if rootFetch.Match(data) {
			t.Errorf("%s contains a fetch that bypasses the document base", name)
		}
	}

	websocketJS, err := staticFS.ReadFile("static/js/websocket.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(websocketJS), "new URL('ws/status', document.baseURI)") {
		t.Error("WebSocket URL is not resolved against the document base")
	}

	tokensCSS, err := staticFS.ReadFile("static/css/v2/tokens.css")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(tokensCSS), "url('/static/") {
		t.Error("font URL bypasses the Ingress base path")
	}
}

func TestNFCURLsHonorConfiguredBasePath(t *testing.T) {
	ws, _, spoolman := newTestServerAtBasePath(t, "/filabridge")
	spoolman.Spools[7] = &fakeSpool{ID: 7, Name: "Violet", RemainingWeight: 750}

	req := httptest.NewRequest(http.MethodGet, "/filabridge/api/nfc/urls", nil)
	req.Host = "172.30.33.4:5000"
	req.Header.Set("X-Forwarded-Host", "ha.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	ws.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("NFC URLs = %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		URLs []struct {
			Type     string `json:"type"`
			URL      string `json:"url"`
			ComboURL string `json:"combo_url"`
		} `json:"urls"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}

	want := "https://ha.example.com/filabridge/api/nfc/assign?spool=7"
	for _, item := range payload.URLs {
		if item.Type == "spool" && item.URL == want {
			if !strings.HasPrefix(item.ComboURL, want+"&location=") {
				t.Errorf("combo URL = %q, want prefix %q", item.ComboURL, want+"&location=")
			}
			return
		}
	}
	t.Errorf("spool URL %q not found in response: %s", want, rec.Body.String())
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

// TestLegacyUIGate: the current interface is served by default, and
// FILABRIDGE_OLD_UI brings back the pre-1.2.2 look. The classic sheets load
// either way, since the current styling layers on top of them rather than
// replacing them, so dropping them would leave the new look with no base.
func TestLegacyUIGate(t *testing.T) {
	// Both the dashboard (which passes LegacyUI explicitly) and a standalone page
	// (which gets it from renderPage) have to honour the flag.
	pages := map[string]string{
		"dashboard":  "/",
		"standalone": "/api/nfc/assign", // no params: renders nfc_error.html
	}

	for name, path := range pages {
		t.Run(name+"/default", func(t *testing.T) {
			t.Setenv("FILABRIDGE_OLD_UI", "")
			ws, _, _ := newTestServer(t)
			rec, _ := doJSON(t, ws, http.MethodGet, path, "")
			body := rec.Body.String()
			if !strings.Contains(body, "/static/css/v2/tokens.css") {
				t.Error("the current interface should be served by default")
			}
			if !strings.Contains(body, `data-theme="dark"`) {
				t.Error("expected the theme hook on <html> by default")
			}
		})

		t.Run(name+"/old ui", func(t *testing.T) {
			t.Setenv("FILABRIDGE_OLD_UI", "true")
			ws, _, _ := newTestServer(t)
			rec, _ := doJSON(t, ws, http.MethodGet, path, "")
			body := rec.Body.String()
			if strings.Contains(body, "/static/css/v2/") {
				t.Error("FILABRIDGE_OLD_UI must drop the current styling")
			}
			if strings.Contains(body, `data-theme="dark"`) {
				t.Error("FILABRIDGE_OLD_UI must drop the theme hook too")
			}
		})
	}

	t.Run("classic sheets always load", func(t *testing.T) {
		for _, oldUI := range []string{"true", ""} {
			t.Setenv("FILABRIDGE_OLD_UI", oldUI)
			ws, _, _ := newTestServer(t)
			rec, _ := doJSON(t, ws, http.MethodGet, "/", "")
			for _, sheet := range []string{"main.css", "components.css", "tabs.css", "nfc.css"} {
				if !strings.Contains(rec.Body.String(), "/static/css/"+sheet) {
					t.Errorf("old ui %q: classic %s is no longer linked", oldUI, sheet)
				}
			}
		}
	})

	// The two flags are independent: the interface must not follow developer
	// mode, which now only gates in-progress features like Bambu support.
	t.Run("independent of developer mode", func(t *testing.T) {
		t.Setenv("FILABRIDGE_DEVELOPER_MODE", "true")
		t.Setenv("FILABRIDGE_OLD_UI", "true")
		ws, _, _ := newTestServer(t)
		rec, _ := doJSON(t, ws, http.MethodGet, "/", "")
		body := rec.Body.String()
		if strings.Contains(body, "/static/css/v2/") {
			t.Error("developer mode must not force the current interface back on")
		}
		if !strings.Contains(body, `data-developer-mode="true"`) {
			t.Error("developer mode should still be exposed for feature gating")
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

// TestRememberPreviousLocationRoundTrip: with the remember mode on, a spool
// returns to the location it was loaded from rather than to the global default,
// while a spool with no remembered location still falls back to the default.
func TestRememberPreviousLocationRoundTrip(t *testing.T) {
	ws, _, spoolman := newTestServer(t)
	spoolman.Spools[1] = &fakeSpool{ID: 1, Name: "Red", RemainingWeight: 500, Location: "Drybox 3"}
	spoolman.Spools[2] = &fakeSpool{ID: 2, Name: "Blue", RemainingWeight: 500} // no location: nothing to remember

	if err := ws.bridge.SetAutoAssignPreviousSpoolEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := ws.bridge.SetAutoAssignPreviousSpoolLocation("Storage"); err != nil {
		t.Fatal(err)
	}
	if err := ws.bridge.SetAutoAssignPreviousSpoolRemember(true); err != nil {
		t.Fatal(err)
	}

	// Spool 1 goes on the toolhead; "Drybox 3" is remembered as where it came from
	doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":1}`)
	if got := spoolman.Spools[1].Location; got != "TestPrinter - Toolhead 0" {
		t.Fatalf("spool 1 location = %q, want toolhead location", got)
	}

	// Spool 2 displaces it, so spool 1 goes home to Drybox 3, not to Storage
	rec, _ := doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("map spool 2: %d %s", rec.Code, rec.Body.String())
	}
	if got := spoolman.Spools[1].Location; got != "Drybox 3" {
		t.Fatalf("displaced spool 1 location = %q, want %q", got, "Drybox 3")
	}

	// Spool 2 had no location to remember, so unmapping falls back to the default
	rec, _ = doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unmap: %d %s", rec.Code, rec.Body.String())
	}
	if got := spoolman.Spools[2].Location; got != "Storage" {
		t.Fatalf("spool 2 location = %q, want fallback to %q", got, "Storage")
	}
}

// TestRememberPreviousLocationNeverRemembersToolhead: a toolhead is not a place
// a spool can be sent back to, so a spool already sitting at a toolhead location
// in Spoolman when it is mapped (imported mappings, or a location set by hand)
// falls back to the default location instead of "returning" to that toolhead.
func TestRememberPreviousLocationNeverRemembersToolhead(t *testing.T) {
	ws, _, spoolman := newTestServer(t)
	spoolman.Spools[1] = &fakeSpool{ID: 1, Name: "Red", RemainingWeight: 500, Location: "TestPrinter - Toolhead 0"}

	if err := ws.bridge.SetAutoAssignPreviousSpoolEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := ws.bridge.SetAutoAssignPreviousSpoolLocation("Storage"); err != nil {
		t.Fatal(err)
	}
	if err := ws.bridge.SetAutoAssignPreviousSpoolRemember(true); err != nil {
		t.Fatal(err)
	}

	doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":1}`)
	rec, _ := doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unmap: %d %s", rec.Code, rec.Body.String())
	}
	if got := spoolman.Spools[1].Location; got != "Storage" {
		t.Fatalf("spool location = %q, want fallback to %q", got, "Storage")
	}
}

// TestRememberPreviousLocationSurvivesRepeatedLoads: a spool's home holds up
// across repeated load/unload cycles rather than drifting to the toolhead.
func TestRememberPreviousLocationSurvivesRepeatedLoads(t *testing.T) {
	ws, _, spoolman := newTestServer(t)
	spoolman.Spools[1] = &fakeSpool{ID: 1, Name: "Red", RemainingWeight: 500, Location: "Drybox 3"}

	if err := ws.bridge.SetAutoAssignPreviousSpoolEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := ws.bridge.SetAutoAssignPreviousSpoolRemember(true); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":1}`)
		rec, _ := doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":0}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("cycle %d unmap: %d %s", i, rec.Code, rec.Body.String())
		}
		if got := spoolman.Spools[1].Location; got != "Drybox 3" {
			t.Fatalf("cycle %d: spool location = %q, want %q", i, got, "Drybox 3")
		}
	}
}

// TestRememberPreviousLocationOffUsesDefault: leaving the remember mode off
// keeps the pre-existing behavior of sending every spool to the one default
// location, even when its previous location is known.
func TestRememberPreviousLocationOffUsesDefault(t *testing.T) {
	ws, _, spoolman := newTestServer(t)
	spoolman.Spools[1] = &fakeSpool{ID: 1, Name: "Red", RemainingWeight: 500, Location: "Drybox 3"}

	if err := ws.bridge.SetAutoAssignPreviousSpoolEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := ws.bridge.SetAutoAssignPreviousSpoolLocation("Storage"); err != nil {
		t.Fatal(err)
	}

	doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":1}`)
	doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":0}`)

	if got := spoolman.Spools[1].Location; got != "Storage" {
		t.Fatalf("spool location = %q, want %q", got, "Storage")
	}
}

// TestScannedStorageLocationBecomesHome: assigning a spool to a storage location
// (the NFC/QR path) makes that its home, so it returns there after its next
// stint on a toolhead.
func TestScannedStorageLocationBecomesHome(t *testing.T) {
	ws, _, spoolman := newTestServer(t)
	spoolman.Spools[1] = &fakeSpool{ID: 1, Name: "Red", RemainingWeight: 500}

	if err := ws.bridge.SetAutoAssignPreviousSpoolEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := ws.bridge.SetAutoAssignPreviousSpoolRemember(true); err != nil {
		t.Fatal(err)
	}

	if err := ws.bridge.AssignSpoolToLocation(1, "", 0, "Shelf A", false); err != nil {
		t.Fatalf("assign to storage location: %v", err)
	}

	doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":1}`)
	doJSON(t, ws, http.MethodPost, "/api/map_toolhead", `{"printer_name":"TestPrinter","toolhead_id":0,"spool_id":0}`)

	if got := spoolman.Spools[1].Location; got != "Shelf A" {
		t.Fatalf("spool location = %q, want %q", got, "Shelf A")
	}
}

// TestLocationsListAndTypeToolheads: /api/locations lists what Spoolman reports
// from /api/v1/location, skipping the blank names that unassigned spools
// produce, and types this instance's toolhead locations "printer" so the
// storage dropdown filters them out.
func TestLocationsListAndTypeToolheads(t *testing.T) {
	ws, _, spoolman := newTestServer(t)
	// "TestPrinter - Toolhead 0" is a toolhead location, "Drybox" is storage.
	// Spoolman reports an unassigned spool's location as "", and whitespace-only
	// names occur in the wild; neither is a place anything can be filed under.
	spoolman.Locations = []string{"Drybox", "", "   ", "TestPrinter - Toolhead 0"}

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
	if types["Drybox"] != "storage" {
		t.Errorf("Drybox type = %q, want storage", types["Drybox"])
	}
	if types["TestPrinter - Toolhead 0"] != "printer" {
		t.Errorf("toolhead location should be typed 'printer'; got %q", types["TestPrinter - Toolhead 0"])
	}
	// Catches both "" and "   ": either would land as an extra, unnamed entry.
	if len(types) != 2 {
		t.Errorf("blank locations must not be listed; want 2 entries, got %d (%v)", len(types), types)
	}
}

// TestMapToolheadRejectsInvalidTargets: mappings to toolheads beyond the
// printer's configured count (or to unknown printers) must be rejected, not
// silently stored.
// TestAssignMappingWarningEndpoint: answering a mapping warning names a
// toolhead, and toolhead 0 is a real answer (it is the default attribution, so
// "yes, slot 1" must not be read as a missing field).
func TestAssignMappingWarningEndpoint(t *testing.T) {
	ws, _, spoolman := newTestServer(t)
	spoolman.Spools[1] = &fakeSpool{ID: 1, Name: "Spool", RemainingWeight: 500}
	ws.bridge.mappingWarnings["mapping_test"] = MappingWarning{
		ID:          "mapping_test",
		PrinterID:   "printer_test",
		PrinterName: "TestPrinter",
		ToolheadID:  0,
		JobID:       7,
		JobName:     "single.bgcode",
		Grams:       102.48,
		Slots:       []MappingWarningSlot{{ToolheadID: 0}, {ToolheadID: 1}},
	}

	rec, body := doJSON(t, ws, http.MethodPost, "/api/mapping-warnings/mapping_test/assign", `{"toolhead_id":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("assigning toolhead 0 must succeed, got %d %s", rec.Code, rec.Body.String())
	}
	warning, _ := body["warning"].(map[string]interface{})
	if warning == nil || warning["assigned"] != true || warning["assigned_toolhead"].(float64) != 0 {
		t.Fatalf("response did not confirm the answer: %v", body)
	}

	rec, _ = doJSON(t, ws, http.MethodPost, "/api/mapping-warnings/mapping_test/assign", `{"toolhead_id":9}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("toolhead the printer does not have must 400, got %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = doJSON(t, ws, http.MethodPost, "/api/mapping-warnings/nope/assign", `{"toolhead_id":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown warning must 400, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestMappingWarningRendersSlotPicker: a warning already on the dashboard when
// the page loads must render the same picker the websocket path builds, since a
// user arriving mid-print has no other way to answer it.
func TestMappingWarningRendersSlotPicker(t *testing.T) {
	ws, _, _ := newTestServer(t)
	ws.bridge.mappingWarnings["mapping_test"] = MappingWarning{
		ID:               "mapping_test",
		PrinterID:        "printer_test",
		PrinterName:      "TestPrinter",
		ToolheadID:       0,
		JobID:            7,
		JobName:          "single.bgcode",
		Grams:            102.48,
		Assigned:         true,
		AssignedToolhead: 1,
		Slots: []MappingWarningSlot{
			{ToolheadID: 0, DisplayName: "Toolhead 0 (slot 1)", SpoolID: 3, SpoolLabel: "[3] PLA - TestVendor - Galaxy Black"},
			{ToolheadID: 1, DisplayName: "Toolhead 1 (slot 2)"},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	ws.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard returned %d", rec.Code)
	}

	html := rec.Body.String()
	for _, want := range []string{
		`id="mapping-slot-mapping_test"`,
		`<option value="0">Toolhead 0 (slot 1) - [3] PLA - TestVendor - Galaxy Black</option>`,
		`<option value="1" selected>Toolhead 1 (slot 2) - no spool mapped</option>`,
		"Recording against Toolhead 1 (slot 2).",
		"No spool is mapped to Toolhead 1 (slot 2)",
		`onclick="assignMappingWarning('mapping_test')"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
}

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

// TestSettingsSubTabsHaveMatchingContent guards the same contract for the
// Settings sub-tabs, which are also persisted in the URL hash as
// "#settings/<sub-tab>": every switchSettingsTab('x') button needs a matching
// content div (id="x-tab", class="settings-tab-content"), otherwise a reload
// quietly drops back to the default sub-tab.
func TestSettingsSubTabsHaveMatchingContent(t *testing.T) {
	ws, _, _ := newTestServer(t)
	rec, _ := doJSON(t, ws, http.MethodGet, "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: %d", rec.Code)
	}
	body := rec.Body.String()

	matches := regexp.MustCompile(`switchSettingsTab\('([^']+)'`).FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		t.Fatal("no settings sub-tab buttons found in rendered page")
	}

	contentDiv := regexp.MustCompile(`<div id="([a-z-]+)-tab" class="settings-tab-content`)
	contents := map[string]bool{}
	for _, m := range contentDiv.FindAllStringSubmatch(body, -1) {
		contents[m[1]] = true
	}

	for _, m := range matches {
		if !contents[m[1]] {
			t.Errorf("settings sub-tab %q has no matching settings-tab-content div", m[1])
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

// nfcScan performs an NFC tag scan against the assign endpoint and returns the
// rendered page body. Query values are escaped for the caller so location names
// with spaces and dashes survive the trip.
func nfcScan(t *testing.T, ws *WebServer, spool string, location string) string {
	t.Helper()
	q := neturl.Values{}
	if spool != "" {
		q.Set("spool", spool)
	}
	if location != "" {
		q.Set("location", location)
	}
	rec, _ := doJSON(t, ws, http.MethodGet, "/api/nfc/assign?"+q.Encode(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("nfc scan (spool=%q location=%q): status %d: %s", spool, location, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// setToolheadUnloadMode turns the "toolhead tag first unloads" workflow on or
// off through the API, the same way the settings page does.
func setToolheadUnloadMode(t *testing.T, ws *WebServer, enabled bool) {
	t.Helper()
	body := `{"enabled":false}`
	if enabled {
		body = `{"enabled":true}`
	}
	rec, _ := doJSON(t, ws, http.MethodPut, "/api/config/nfc-toolhead-unload", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("set toolhead unload mode: status %d: %s", rec.Code, rec.Body.String())
	}
}

// mappedSpool returns the spool currently mapped to the test printer's only
// toolhead, or 0 when it is empty.
func mappedSpool(t *testing.T, ws *WebServer) int {
	t.Helper()
	spoolID, err := ws.bridge.GetToolheadMapping("TestPrinter", 0)
	if err != nil {
		t.Fatalf("GetToolheadMapping: %v", err)
	}
	return spoolID
}

// TestNFCToolheadFirstUnload covers the optional workflow where a toolhead tag
// scanned on its own unloads that toolhead (issue #42), including the guarantee
// that everything else about NFC scanning is unchanged while it is off.
func TestNFCToolheadFirstUnload(t *testing.T) {
	const toolhead = "TestPrinter - Toolhead 0"

	// Each subtest gets its own server: NFC sessions are keyed by client IP,
	// which is identical across requests here, so a shared server would leak
	// half-finished sessions from one case into the next.
	setup := func(t *testing.T) (*WebServer, *fakeSpoolman) {
		t.Helper()
		ws, _, spoolman := newTestServer(t)
		spoolman.Spools[7] = &fakeSpool{ID: 7, Name: "Galaxy Black", RemainingWeight: 800}
		return ws, spoolman
	}

	t.Run("off: toolhead first still waits for a spool", func(t *testing.T) {
		ws, _ := setup(t)
		if err := ws.bridge.SetToolheadMapping("TestPrinter", 0, 7); err != nil {
			t.Fatalf("SetToolheadMapping: %v", err)
		}

		body := nfcScan(t, ws, "", toolhead)
		if !strings.Contains(body, "Now scan a spool tag") {
			t.Fatalf("expected the scan-in-any-order progress page, got: %s", body)
		}
		if got := mappedSpool(t, ws); got != 7 {
			t.Fatalf("toolhead mapping = %d, want it left alone at 7", got)
		}
	})

	t.Run("on: toolhead first unloads the loaded spool", func(t *testing.T) {
		ws, spoolman := setup(t)
		setToolheadUnloadMode(t, ws, true)
		if err := ws.bridge.SetToolheadMapping("TestPrinter", 0, 7); err != nil {
			t.Fatalf("SetToolheadMapping: %v", err)
		}

		body := nfcScan(t, ws, "", toolhead)
		if !strings.Contains(body, "Toolhead Unloaded") {
			t.Fatalf("expected the unload page, got: %s", body)
		}
		if got := mappedSpool(t, ws); got != 0 {
			t.Fatalf("toolhead mapping = %d, want it cleared", got)
		}
		// The spool left the toolhead in Spoolman too, not just in FilaBridge.
		if got := spoolman.Spools[7].Location; got == toolhead {
			t.Fatalf("spool 7 still sits at %q in Spoolman", got)
		}
	})

	t.Run("on: empty toolhead says so instead of claiming an unload", func(t *testing.T) {
		ws, _ := setup(t)
		setToolheadUnloadMode(t, ws, true)

		body := nfcScan(t, ws, "", toolhead)
		if !strings.Contains(body, "Toolhead Was Empty") {
			t.Fatalf("expected the empty-toolhead page, got: %s", body)
		}
		if strings.Contains(body, "Toolhead Unloaded") {
			t.Fatal("empty toolhead reported as an unload")
		}
	})

	t.Run("on: spool then toolhead still loads", func(t *testing.T) {
		ws, _ := setup(t)
		setToolheadUnloadMode(t, ws, true)

		if body := nfcScan(t, ws, "7", ""); !strings.Contains(body, "Now scan a location tag") {
			t.Fatalf("expected the progress page after a spool scan, got: %s", body)
		}
		if body := nfcScan(t, ws, "", toolhead); !strings.Contains(body, "Assignment Complete") {
			t.Fatalf("expected the assignment page, got: %s", body)
		}
		if got := mappedSpool(t, ws); got != 7 {
			t.Fatalf("toolhead mapping = %d, want spool 7 loaded", got)
		}
	})

	t.Run("on: single-scan combo URL still loads", func(t *testing.T) {
		ws, _ := setup(t)
		setToolheadUnloadMode(t, ws, true)

		if body := nfcScan(t, ws, "7", toolhead); !strings.Contains(body, "Assignment Complete") {
			t.Fatalf("expected the assignment page, got: %s", body)
		}
		if got := mappedSpool(t, ws); got != 7 {
			t.Fatalf("toolhead mapping = %d, want spool 7 loaded", got)
		}
	})

	t.Run("on: storage location tags are unaffected", func(t *testing.T) {
		ws, _ := setup(t)
		setToolheadUnloadMode(t, ws, true)

		body := nfcScan(t, ws, "", "Drybox")
		if !strings.Contains(body, "Now scan a spool tag") {
			t.Fatalf("expected a storage location to wait for a spool, got: %s", body)
		}
	})
}

// TestNFCToolheadUnloadSettingRoundTrip: the setting defaults to off, so an
// existing install keeps the scan-in-any-order workflow until it opts in.
func TestNFCToolheadUnloadSettingRoundTrip(t *testing.T) {
	ws, _, _ := newTestServer(t)

	_, body := doJSON(t, ws, http.MethodGet, "/api/config/nfc-toolhead-unload", "")
	if body["enabled"] != false {
		t.Fatalf("enabled = %v, want false by default", body["enabled"])
	}

	setToolheadUnloadMode(t, ws, true)
	_, body = doJSON(t, ws, http.MethodGet, "/api/config/nfc-toolhead-unload", "")
	if body["enabled"] != true {
		t.Fatalf("enabled = %v, want true after opting in", body["enabled"])
	}

	// Turning it back off has to work: a bool with binding:"required" would
	// reject false as a missing value and strand the user in the new workflow.
	setToolheadUnloadMode(t, ws, false)
	_, body = doJSON(t, ws, http.MethodGet, "/api/config/nfc-toolhead-unload", "")
	if body["enabled"] != false {
		t.Fatalf("enabled = %v, want false after opting back out", body["enabled"])
	}
}
