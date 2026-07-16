package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHistoryToggleDefaultsOn(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	bridge := newTestBridge(t, printer, spoolman)

	if enabled, err := bridge.GetPrintHistoryEnabled(); err != nil || !enabled {
		t.Fatalf("fresh install: enabled=%v err=%v, want true", enabled, err)
	}

	// Databases from before the setting existed have no row at all: still on
	if _, err := bridge.db.Exec("DELETE FROM configuration WHERE key = ?", ConfigKeyPrintHistoryEnabled); err != nil {
		t.Fatal(err)
	}
	if enabled, err := bridge.GetPrintHistoryEnabled(); err != nil || !enabled {
		t.Fatalf("missing key: enabled=%v err=%v, want true", enabled, err)
	}
}

// TestHistoryDisabledSkipsWritesButStillRecords is the core contract: the
// toggle controls only the local log, never the Spoolman recording.
func TestHistoryDisabledSkipsWritesButStillRecords(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[1] = &fakeSpool{ID: 1, Name: "Spool", RemainingWeight: 750}
	bridge := newTestBridge(t, printer, spoolman)
	bridge.SetToolheadMapping("TestPrinter", 0, 1)
	bridge.SetPrintHistoryEnabled(false)

	printer.set(func(f *fakePrusaLink) {
		f.State = "PRINTING"
		f.JobID = 60
		f.Filename = "part.bgcode"
		f.FileBody = bgcodeFixture("25.0", 1024)
	})
	cycle(t, bridge)
	printer.set(func(f *fakePrusaLink) { f.State = "FINISHED" })
	cycle(t, bridge)

	if len(spoolman.PatchCalls) != 1 {
		t.Fatalf("Spoolman recording must be unaffected by the toggle: %d patches", len(spoolman.PatchCalls))
	}
	if history, _ := bridge.GetPrintHistory(10); len(history) != 0 {
		t.Fatalf("history written while disabled: %+v", history)
	}

	// Re-enable: the next print logs again
	bridge.SetPrintHistoryEnabled(true)
	printer.set(func(f *fakePrusaLink) { f.State = "PRINTING"; f.JobID = 61; f.Progress = 0 })
	cycle(t, bridge)
	printer.set(func(f *fakePrusaLink) { f.State = "FINISHED" })
	cycle(t, bridge)

	if history, _ := bridge.GetPrintHistory(10); len(history) != 1 {
		t.Fatalf("history not written after re-enable: %+v", history)
	}
}

// TestClearHistoryPreservesDedupLedger: clearing the viewing log must never
// touch recorded_jobs, or cleared jobs could be double-counted.
func TestClearHistoryPreservesDedupLedger(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[1] = &fakeSpool{ID: 1, Name: "Spool", RemainingWeight: 750}
	bridge := newTestBridge(t, printer, spoolman)
	bridge.SetToolheadMapping("TestPrinter", 0, 1)

	printer.set(func(f *fakePrusaLink) {
		f.State = "PRINTING"
		f.JobID = 62
		f.Filename = "part.bgcode"
		f.FileBody = bgcodeFixture("25.0", 1024)
	})
	cycle(t, bridge)
	printer.set(func(f *fakePrusaLink) { f.State = "FINISHED" })
	cycle(t, bridge)

	if err := bridge.ClearPrintHistory(); err != nil {
		t.Fatal(err)
	}
	if history, _ := bridge.GetPrintHistory(10); len(history) != 0 {
		t.Fatal("history not cleared")
	}
	if recorded, _ := bridge.isJobRecorded("printer_test", 62); !recorded {
		t.Fatal("clear touched the dedup ledger")
	}

	// The dedup must still hold: the job reappearing records nothing new
	printer.set(func(f *fakePrusaLink) { f.State = "PRINTING"; f.Progress = 50 })
	cycle(t, bridge)
	printer.set(func(f *fakePrusaLink) { f.State = "FINISHED" })
	cycle(t, bridge)
	if len(spoolman.PatchCalls) != 1 {
		t.Fatalf("cleared job was double-counted: %d patches", len(spoolman.PatchCalls))
	}
}

// TestHistorySettingAPIAcceptsFalse guards the gin binding:"required" bool
// trap on this endpoint too: disabling must be possible.
func TestHistorySettingAPIAcceptsFalse(t *testing.T) {
	ws, _, _ := newTestServer(t)

	rec, _ := doJSON(t, ws, http.MethodPut, "/api/config/print-history", `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disabling must be possible: %d %s", rec.Code, rec.Body.String())
	}
	rec, body := doJSON(t, ws, http.MethodGet, "/api/config/print-history", "")
	if rec.Code != http.StatusOK || body["enabled"] != false {
		t.Fatalf("after disable: %d %v", rec.Code, body)
	}

	// Clear endpoint
	ws.bridge.LogPrintUsage("P", 0, 1, 5.0, "j.gcode", time.Now(), "completed")
	rec, _ = doJSON(t, ws, http.MethodDelete, "/api/print-history", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: %d", rec.Code)
	}
	if history, _ := ws.bridge.GetPrintHistory(10); len(history) != 0 {
		t.Fatal("DELETE did not clear history")
	}
}

// TestDashboardHidesHistoryTab verifies the tab is server-side conditional:
// absent from the HTML when disabled, not merely CSS-hidden.
func TestDashboardHidesHistoryTab(t *testing.T) {
	ws, _, _ := newTestServer(t)

	rec, _ := doJSON(t, ws, http.MethodGet, "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "switchTab('history')") {
		t.Fatal("history tab missing while enabled")
	}

	ws.bridge.SetPrintHistoryEnabled(false)
	rec, _ = doJSON(t, ws, http.MethodGet, "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "switchTab('history')") {
		t.Fatal("history tab still rendered while disabled")
	}
}
