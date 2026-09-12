package main

import (
	"bytes"
	"database/sql"
	"log"
	"os"
	"strings"
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

// TestMappingsRekeyedByPrinterID: mappings used to be stored against the
// printer's name, so renaming a printer orphaned them. They move to the
// printer's id, which never changes. A mapping whose printer is gone has no id
// to move to and is set aside rather than dropped, so its spool stops being
// claimed by a printer that no longer exists but is still recoverable.
func TestMappingsRekeyedByPrinterID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FILABRIDGE_DB_PATH", dir)

	db, err := sql.Open("sqlite", dir+"/"+DefaultDBFileName)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE printer_configs (printer_id TEXT PRIMARY KEY, name TEXT NOT NULL, ip_address TEXT NOT NULL, api_key TEXT, toolheads INTEGER DEFAULT 1, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO printer_configs (printer_id, name, ip_address, api_key, toolheads) VALUES ('p1', 'Workhorse', '10.0.0.5', 'k', 2);
		CREATE TABLE toolhead_mappings (printer_name TEXT, toolhead_id INTEGER, spool_id INTEGER, mapped_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, PRIMARY KEY (printer_name, toolhead_id));
		INSERT INTO toolhead_mappings (printer_name, toolhead_id, spool_id, mapped_at) VALUES ('Workhorse', 0, 7, CURRENT_TIMESTAMP);
		INSERT INTO toolhead_mappings (printer_name, toolhead_id, spool_id, mapped_at) VALUES ('Workhorse', 1, 8, CURRENT_TIMESTAMP);
		INSERT INTO toolhead_mappings (printer_name, toolhead_id, spool_id, mapped_at) VALUES ('Sold Long Ago', 0, 9, CURRENT_TIMESTAMP);
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

	for toolhead, wantSpool := range map[int]int{0: 7, 1: 8} {
		if got, err := bridge.GetToolheadMapping("Workhorse", toolhead); err != nil || got != wantSpool {
			t.Errorf("toolhead %d = %d (%v), want %d", toolhead, got, err, wantSpool)
		}
	}

	var rekeyed int
	if err := bridge.db.QueryRow(`SELECT COUNT(*) FROM toolhead_mappings WHERE printer_id = 'p1'`).Scan(&rekeyed); err != nil {
		t.Fatal(err)
	}
	if rekeyed != 2 {
		t.Errorf("%d rows keyed by printer id, want 2", rekeyed)
	}

	var orphanSpool int
	if err := bridge.db.QueryRow(`SELECT spool_id FROM toolhead_mappings_orphaned WHERE printer_name = 'Sold Long Ago'`).Scan(&orphanSpool); err != nil {
		t.Fatalf("mapping for a deleted printer was dropped instead of set aside: %v", err)
	}
	if orphanSpool != 9 {
		t.Errorf("orphaned spool = %d, want 9", orphanSpool)
	}
	// The spool is free again: nothing claims it.
	var stillClaimed int
	if err := bridge.db.QueryRow(`SELECT COUNT(*) FROM toolhead_mappings WHERE spool_id = 9`).Scan(&stillClaimed); err != nil {
		t.Fatal(err)
	}
	if stillClaimed != 0 {
		t.Errorf("spool 9 is still claimed by a printer that no longer exists")
	}

	// Opening the database again must not migrate a second time.
	bridge.Close()
	again, err := NewFilamentBridge(nil)
	if err != nil {
		t.Fatalf("reopening migrated database failed: %v", err)
	}
	defer again.Close()
	if got, err := again.GetToolheadMapping("Workhorse", 1); err != nil || got != 8 {
		t.Errorf("after reopening, toolhead 1 = %d (%v), want 8", got, err)
	}
}

// TestMappingsSurviveRenameAndNotReuse: renaming a printer keeps its mappings,
// and deleting one then adding another with the same name starts empty rather
// than inheriting spools that belong to hardware that is gone.
func TestMappingsSurviveRenameAndNotReuse(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[4] = &fakeSpool{ID: 4, Name: "Red", RemainingWeight: 500}
	b := newTestBridge(t, printer, spoolman)

	if err := b.SetToolheadMapping("TestPrinter", 0, 4); err != nil {
		t.Fatal(err)
	}

	// Rename in place: same printer id, new name.
	cfg := b.GetConfigSnapshot().Printers["printer_test"]
	cfg.Name = "Renamed Printer"
	if err := b.SavePrinterConfig("printer_test", cfg); err != nil {
		t.Fatal(err)
	}
	if err := b.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	if got, err := b.GetToolheadMapping("Renamed Printer", 0); err != nil || got != 4 {
		t.Errorf("rename lost the mapping: got %d (%v), want 4", got, err)
	}

	// Delete it, then add a different printer that happens to reuse the name.
	if err := b.DeletePrinterConfig("printer_test"); err != nil {
		t.Fatal(err)
	}
	if err := b.SavePrinterConfig("printer_new", PrinterConfig{Name: "Renamed Printer", IPAddress: "127.0.0.1:2", APIKey: "k", Toolheads: 1}); err != nil {
		t.Fatal(err)
	}
	if err := b.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	if got, err := b.GetToolheadMapping("Renamed Printer", 0); err != nil || got != 0 {
		t.Errorf("new printer inherited a deleted printer's spool: got %d (%v), want 0", got, err)
	}

	// The deleted printer must also stop claiming its spool, or the spool could
	// never be mapped anywhere again: one spool can only be on one toolhead, and
	// the row holding it belongs to hardware that no longer exists.
	if err := b.SetToolheadMapping("Renamed Printer", 0, 4); err != nil {
		t.Errorf("spool 4 is still claimed by the deleted printer: %v", err)
	}
}

// TestActiveJobsEstimateSlotsMigration: a database written before the slicer's
// slot count was recorded must keep its in-flight job, and that job reads back
// with 0 slots, which is what keeps pre-upgrade jobs on the old warning
// behaviour instead of silently changing what they attribute.
func TestActiveJobsEstimateSlotsMigration(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FILABRIDGE_DB_PATH", dir)

	db, err := sql.Open("sqlite", dir+"/"+DefaultDBFileName)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE active_jobs (printer_id TEXT PRIMARY KEY, job_id INTEGER NOT NULL DEFAULT 0, filename TEXT NOT NULL DEFAULT '', last_progress REAL NOT NULL DEFAULT 0, started_at TIMESTAMP NOT NULL, usage_json TEXT NOT NULL DEFAULT '', updated_at TIMESTAMP NOT NULL);
		INSERT INTO active_jobs (printer_id, job_id, filename, last_progress, started_at, usage_json, updated_at)
		VALUES ('printer_test', 11, 'usb/mid_print.bgcode', 0.4, CURRENT_TIMESTAMP, '{"0":42.5}', CURRENT_TIMESTAMP);
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

	aj, err := bridge.getActiveJob("printer_test")
	if err != nil {
		t.Fatalf("getActiveJob after migration: %v", err)
	}
	if aj == nil {
		t.Fatal("in-flight job lost by the migration")
	}
	if aj.JobID != 11 || aj.Usage[0] != 42.5 {
		t.Errorf("job mangled by migration: %+v", aj)
	}
	if aj.EstimateSlots != 0 {
		t.Errorf("pre-upgrade job reports %d slots, want 0", aj.EstimateSlots)
	}

	// And the column is writable, so the next capture records the count.
	aj.EstimateSlots = 5
	if err := bridge.upsertActiveJob(aj); err != nil {
		t.Fatalf("upsert after migration: %v", err)
	}
	reread, err := bridge.getActiveJob("printer_test")
	if err != nil || reread.EstimateSlots != 5 {
		t.Errorf("slot count did not survive the round trip: %+v %v", reread, err)
	}
}

// TestUnmapToolheadLogsOnlyRealUnmaps: unmapping a toolhead that holds nothing
// is a no-op and stays silent. It used to log a real-looking "Unmapped" line
// either way, so duplicate unassign requests read as repeated genuine unmaps.
func TestUnmapToolheadLogsOnlyRealUnmaps(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[1] = &fakeSpool{ID: 1, Name: "Red", RemainingWeight: 500}
	bridge := newTestBridge(t, printer, spoolman)

	if err := bridge.SetToolheadMapping("TestPrinter", 0, 1); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	if err := bridge.UnmapToolhead("TestPrinter", 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "Unmapped TestPrinter toolhead 0") {
		t.Fatalf("a real unmap must be logged, got: %q", logged.String())
	}

	// The same request again removes nothing, so it must not claim otherwise.
	logged.Reset()
	if err := bridge.UnmapToolhead("TestPrinter", 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logged.String(), "Unmapped") {
		t.Errorf("unmapping an empty toolhead logged an unmap: %q", logged.String())
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
