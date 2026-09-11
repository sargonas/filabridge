package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata golden files instead of comparing against them")

const prusaCompatGolden = "testdata/prusa_compat.golden"

// TestPrusaCompatGolden pins every place a PrusaLink printer's toolheads reach
// the outside world: Spoolman location strings, what a scanned NFC tag resolves
// to, the API and websocket payloads the UI reads, the dashboard rows, and the
// webhook payloads. The filament positions rework must leave all of it byte
// identical for PrusaLink, so any diff in the golden file is a regression unless
// it is a reviewed, deliberate addition. Regenerate with:
//
//	go test -run TestPrusaCompatGolden -update
func TestPrusaCompatGolden(t *testing.T) {
	ws, _, spoolman := newTestServer(t)
	b := ws.bridge

	// A Prusa XL shaped printer: five toolheads, a custom name on one, spools
	// mapped to three, and a history row.
	if err := b.SavePrinterConfig("printer_xl", PrinterConfig{Name: "XL", IPAddress: "127.0.0.1:2", APIKey: "k", Toolheads: 5}); err != nil {
		t.Fatal(err)
	}
	if err := b.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{11, 12, 13, 14} {
		spoolman.Spools[id] = &fakeSpool{ID: id, Name: fmt.Sprintf("Spool %d", id), RemainingWeight: 500}
	}
	if err := b.SetToolheadName("printer_xl", 2, "Silk Head"); err != nil {
		t.Fatal(err)
	}
	for tid, spool := range map[int]int{0: 11, 1: 12, 4: 14} {
		if err := b.SetToolheadMapping("XL", tid, spool); err != nil {
			t.Fatalf("map XL/%d: %v", tid, err)
		}
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := b.LogPrintUsage("XL", 4, 14, 12.5, "part.bgcode", at, "completed"); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	section := func(name string) { fmt.Fprintf(&out, "\n== %s ==\n", name) }

	section("toolheadLocationName")
	for tid := 0; tid < 5; tid++ {
		fmt.Fprintf(&out, "%d %q\n", tid, b.toolheadLocationName("XL", tid))
	}

	section("toolheadLocationSet")
	var set []string
	for name := range b.toolheadLocationSet() {
		set = append(set, name)
	}
	sort.Strings(set)
	for _, name := range set {
		fmt.Fprintf(&out, "%q\n", name)
	}

	section("parseLocationParam")
	for _, loc := range []string{
		"XL - Toolhead 0",
		"XL - Toolhead 4",
		"XL - Silk Head",
		"XL - Toolhead 2",
		"XL - Toolhead 5",
		"XL - Toolhead 7",
		"TestPrinter - Toolhead 0",
		"Ghost - Toolhead 0",
		"Drybox",
	} {
		printer, tid, locName, isPrinter, err := b.parseLocationParam(loc)
		fmt.Fprintf(&out, "%q -> printer=%q toolhead=%d location=%q printer_location=%v err=%v\n",
			loc, printer, tid, locName, isPrinter, err)
	}

	section("GetStatus toolhead_mappings")
	status, err := b.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	out.WriteString(goldenJSON(t, status.ToolheadMappings))

	section("GET /api/printers")
	rec, _ := doJSON(t, ws, http.MethodGet, "/api/printers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/printers: %d", rec.Code)
	}
	var printers interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &printers); err != nil {
		t.Fatal(err)
	}
	out.WriteString(goldenJSON(t, printers))

	section("buildMappingSlots")
	slots := b.buildMappingSlots("printer_xl", "XL", 5)
	out.WriteString(goldenJSON(t, slots))

	section("print history")
	history, err := b.GetPrintHistory(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range history {
		fmt.Fprintf(&out, "printer=%q toolhead=%d spool=%d grams=%.2f job=%q status=%q\n",
			h.PrinterName, h.ToolheadID, h.SpoolID, h.FilamentUsed, h.JobName, h.Status)
	}

	section("webhook payloads")
	out.WriteString(goldenJSON(t, lowFilamentPayload(RunoutWarning{
		ID: "runout_xl", PrinterID: "printer_xl", PrinterName: "XL", ToolheadID: 4,
		SpoolID: 14, SpoolName: "Spool 14", JobID: 9, RequiredWeight: 80, RemainingWeight: 20, AutoPaused: true,
	}, at)))
	out.WriteString(goldenJSON(t, mappingWarningPayload("XL", "part.bgcode", 2, 33.3, at)))

	section("dashboard")
	b.warnMutex.Lock()
	b.runoutWarnings["runout_xl"] = RunoutWarning{
		ID: "runout_xl", PrinterID: "printer_xl", PrinterName: "XL", ToolheadID: 4,
		SpoolID: 14, SpoolName: "Spool 14", JobID: 9, RequiredWeight: 80, RemainingWeight: 20, Timestamp: at,
	}
	b.mappingWarnings["mapping_xl"] = MappingWarning{
		ID: "mapping_xl", PrinterID: "printer_xl", PrinterName: "XL", ToolheadID: 2,
		JobID: 9, JobName: "part.bgcode", Grams: 33.3, Slots: slots, Timestamp: at,
	}
	b.warnMutex.Unlock()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	page := httptest.NewRecorder()
	ws.router.ServeHTTP(page, req)
	if page.Code != http.StatusOK {
		t.Fatalf("dashboard: %d", page.Code)
	}
	for _, line := range dashboardToolheadLines(page.Body.String()) {
		out.WriteString(line + "\n")
	}

	section("ImportMappingsFromSpoolman")
	spoolman.Spools[13].Location = "XL - Silk Head"
	summary, err := b.ImportMappingsFromSpoolman("XL")
	if err != nil {
		t.Fatal(err)
	}
	out.WriteString(goldenJSON(t, summary))
	mappings, err := b.GetToolheadMappings("XL")
	if err != nil {
		t.Fatal(err)
	}
	for tid := 0; tid < 5; tid++ {
		fmt.Fprintf(&out, "XL/%d -> spool %d\n", tid, mappings[tid].SpoolID)
	}

	compareGolden(t, prusaCompatGolden, out.String())
}

// dashboardToolheadLines pulls the rendered markup that carries toolhead ids or
// labels out of the dashboard, in document order: the mapping rows and their
// labels, the low filament warning's printer line, and the mapping warning's
// answer and options.
func dashboardToolheadLines(html string) []string {
	var lines []string
	line := regexp.MustCompile(`<div class="toolhead-mapping-row"[^>]*>|<div class="toolhead-label">[^<]*</div>|<p><strong>Printer:</strong>[^<]*</p>|<p><strong>Recording against [^<]*</strong></p>`)
	for _, m := range line.FindAllString(html, -1) {
		lines = append(lines, m)
	}
	selects := regexp.MustCompile(`(?s)<select class="mapping-slot-select".*?</select>`)
	option := regexp.MustCompile(`<option value="[^"]*"[^>]*>[^<]*</option>`)
	for _, sel := range selects.FindAllString(html, -1) {
		lines = append(lines, option.FindAllString(sel, -1)...)
	}
	return lines
}

// goldenJSON renders v as indented JSON with keys sorted and the values that
// change from run to run (times, the fake printer's port) replaced, so only
// the shape and the stable values are compared.
func goldenJSON(t *testing.T, v interface{}) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var generic interface{}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	normalizeGolden(generic)
	pretty, err := json.MarshalIndent(generic, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(pretty) + "\n"
}

var goldenVolatileKeys = map[string]bool{
	"mapped_at": true, "timestamp": true, "ip_address": true,
	"created_at": true, "updated_at": true, "print_started": true, "print_finished": true,
}

func normalizeGolden(v interface{}) {
	switch v := v.(type) {
	case map[string]interface{}:
		for k, child := range v {
			if goldenVolatileKeys[k] {
				v[k] = "<volatile>"
				continue
			}
			normalizeGolden(child)
		}
	case []interface{}:
		for _, child := range v {
			normalizeGolden(child)
		}
	}
}

// compareGolden fails when got differs from the golden file, or rewrites the
// file under -update.
func compareGolden(t *testing.T, path, got string) {
	t.Helper()
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s (run with -update to create it): %v", path, err)
	}
	if string(want) != got {
		t.Errorf("%s differs from current output.\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}
