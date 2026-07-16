package main

import (
	"database/sql"
	"testing"
)

// TestSchemaMigrations opens a database laid out like a pre-release build
// (billed_jobs table, model column, no status column) and verifies all
// in-place migrations run and preserve data.
func TestSchemaMigrations(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FILABRIDGE_DB_PATH", dir)

	db, err := sql.Open("sqlite", dir+"/"+DefaultDBFileName)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE printer_configs (printer_id TEXT PRIMARY KEY, name TEXT NOT NULL, model TEXT, ip_address TEXT NOT NULL, api_key TEXT, toolheads INTEGER DEFAULT 1, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO printer_configs VALUES ('p1', 'Old', 'MK4', '10.0.0.5', 'k', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);
		CREATE TABLE print_history (id INTEGER PRIMARY KEY AUTOINCREMENT, printer_name TEXT, toolhead_id INTEGER, spool_id INTEGER, filament_used REAL, print_started TIMESTAMP, print_finished TIMESTAMP, job_name TEXT);
		INSERT INTO print_history (printer_name, toolhead_id, spool_id, filament_used, print_started, print_finished, job_name) VALUES ('Old', 0, 1, 5.5, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 'legacy.gcode');
		CREATE TABLE billed_jobs (printer_id TEXT NOT NULL, job_id INTEGER NOT NULL, filename TEXT NOT NULL DEFAULT '', scale REAL NOT NULL DEFAULT 1, billed_at TIMESTAMP NOT NULL, PRIMARY KEY (printer_id, job_id));
		INSERT INTO billed_jobs VALUES ('p1', 7, 'usb/old.bgcode', 0.5, CURRENT_TIMESTAMP);
	`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	bridge, err := NewFilamentBridge(nil)
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	defer bridge.Close()

	// billed_jobs renamed with data preserved
	var scale float64
	if err := bridge.db.QueryRow(`SELECT scale FROM recorded_jobs WHERE printer_id = 'p1' AND job_id = 7`).Scan(&scale); err != nil {
		t.Fatalf("recorded_jobs migration: %v", err)
	}
	if scale != 0.5 {
		t.Errorf("scale = %v, want 0.5", scale)
	}
	if recorded, _ := bridge.isJobRecorded("p1", 7); !recorded {
		t.Error("legacy dedup entry not honored after migration")
	}

	// model column dropped, config still loads
	configs, err := bridge.GetAllPrinterConfigs()
	if err != nil {
		t.Fatalf("GetAllPrinterConfigs after model drop: %v", err)
	}
	if configs["p1"].Name != "Old" || configs["p1"].Toolheads != 1 {
		t.Errorf("printer config mangled by migration: %+v", configs["p1"])
	}

	// status column added, legacy rows default to completed
	history, err := bridge.GetPrintHistory(10)
	if err != nil {
		t.Fatalf("GetPrintHistory: %v", err)
	}
	if len(history) != 1 || history[0].Status != "completed" {
		t.Errorf("legacy history migration: %+v", history)
	}
}

// TestCompletedPrintRecordsViaHeaderScan walks the primary happy path on a
// printer that serves no API metadata (Core One behavior): the estimate is
// captured at print start by streaming the file header, and completion records
// the full amount with no further fetches.
func TestCompletedPrintRecordsViaHeaderScan(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[1] = &fakeSpool{ID: 1, Name: "Big Spool", RemainingWeight: 750, UsedWeight: 250}
	bridge := newTestBridge(t, printer, spoolman)

	if err := bridge.SetToolheadMapping("TestPrinter", 0, 1); err != nil {
		t.Fatal(err)
	}

	printer.set(func(f *fakePrusaLink) {
		f.State = "PRINTING"
		f.JobID = 42
		f.Progress = 5
		f.Filename = "part.bgcode"
		f.FileBody = bgcodeFixture("372.68", 256<<10)
	})
	cycle(t, bridge)

	active, err := bridge.getActiveJob("printer_test")
	if err != nil || active == nil {
		t.Fatalf("active job not tracked: %v %v", active, err)
	}
	if active.Usage[0] != 372.68 {
		t.Fatalf("estimate not captured at print start: %v", active.Usage)
	}

	printer.set(func(f *fakePrusaLink) { f.State = "FINISHED" })
	cycle(t, bridge)

	if got := spoolman.Spools[1].UsedWeight; got < 622.67 || got > 622.69 {
		t.Errorf("spool used_weight = %v, want ~622.68", got)
	}
	history, _ := bridge.GetPrintHistory(10)
	if len(history) != 1 || history[0].Status != "completed" || history[0].FilamentUsed != 372.68 {
		t.Errorf("history: %+v", history)
	}

	// Dedup: the same job reappearing must not record twice
	printer.set(func(f *fakePrusaLink) { f.State = "PRINTING"; f.Progress = 99 })
	cycle(t, bridge)
	printer.set(func(f *fakePrusaLink) { f.State = "FINISHED" })
	cycle(t, bridge)

	if len(spoolman.PatchCalls) != 1 {
		t.Errorf("job recorded %d times, want exactly 1", len(spoolman.PatchCalls))
	}
}

// TestCancelledPrintScaledByProgress verifies a cancelled print records the
// estimate scaled to its last-seen progress.
func TestCancelledPrintScaledByProgress(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[1] = &fakeSpool{ID: 1, Name: "Spool", RemainingWeight: 750, UsedWeight: 0}
	bridge := newTestBridge(t, printer, spoolman)
	bridge.SetToolheadMapping("TestPrinter", 0, 1)

	printer.set(func(f *fakePrusaLink) {
		f.State = "PRINTING"
		f.JobID = 43
		f.Progress = 40
		f.Filename = "part.bgcode"
		f.FileBody = bgcodeFixture("100.0", 1024)
	})
	cycle(t, bridge)

	printer.set(func(f *fakePrusaLink) { f.State = "STOPPED" })
	cycle(t, bridge)

	history, _ := bridge.GetPrintHistory(10)
	if len(history) != 1 {
		t.Fatalf("expected 1 history row, got %d", len(history))
	}
	if history[0].Status != "cancelled" || history[0].FilamentUsed != 40 {
		t.Errorf("cancelled print recorded %v (%s), want 40g (cancelled)", history[0].FilamentUsed, history[0].Status)
	}
}

// TestAttentionKeepsJobInFlight verifies the ATTENTION state (filament runout,
// crash detection) neither drops tracking nor triggers recording.
func TestAttentionKeepsJobInFlight(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[1] = &fakeSpool{ID: 1, Name: "Spool", RemainingWeight: 750}
	bridge := newTestBridge(t, printer, spoolman)
	bridge.SetToolheadMapping("TestPrinter", 0, 1)

	printer.set(func(f *fakePrusaLink) {
		f.State = "PRINTING"
		f.JobID = 44
		f.Progress = 60
		f.Filename = "part.bgcode"
		f.FileBody = bgcodeFixture("50.0", 1024)
	})
	cycle(t, bridge)

	printer.set(func(f *fakePrusaLink) { f.State = "ATTENTION" })
	cycle(t, bridge)

	if active, _ := bridge.getActiveJob("printer_test"); active == nil || active.Usage[0] != 50.0 {
		t.Fatal("tracking lost during ATTENTION state")
	}
	if history, _ := bridge.GetPrintHistory(10); len(history) != 0 {
		t.Fatal("ATTENTION must not trigger recording")
	}

	printer.set(func(f *fakePrusaLink) { f.State = "FINISHED" })
	cycle(t, bridge)
	if history, _ := bridge.GetPrintHistory(10); len(history) != 1 || history[0].FilamentUsed != 50.0 {
		t.Fatalf("print after ATTENTION did not record: %+v", history)
	}
}

// TestMetadataFromAPIPreferred verifies that when the printer serves job
// metadata, no file scan is needed at all.
func TestMetadataFromAPIPreferred(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[1] = &fakeSpool{ID: 1, Name: "Spool", RemainingWeight: 750}
	bridge := newTestBridge(t, printer, spoolman)
	bridge.SetToolheadMapping("TestPrinter", 0, 1)

	printer.set(func(f *fakePrusaLink) {
		f.State = "PRINTING"
		f.JobID = 45
		f.Filename = "part.bgcode"
		f.Meta = map[string]interface{}{"filament used [g]": "20.0"}
		f.BlockFiles = true // any file access would 409 - metadata must suffice
	})
	cycle(t, bridge)

	if active, _ := bridge.getActiveJob("printer_test"); active == nil || active.Usage[0] != 20.0 {
		t.Fatal("API metadata not captured")
	}
}
