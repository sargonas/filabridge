package main

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// positionSummary renders a printer's positions compactly for comparison.
func positionSummary(t *testing.T, b *FilamentBridge, printerID string) []string {
	t.Helper()
	positions, err := b.listPositions(printerID)
	if err != nil {
		t.Fatalf("listPositions: %v", err)
	}
	var out []string
	for _, p := range positions {
		state := "present"
		if !p.Present {
			state = "absent"
		}
		out = append(out, fmt.Sprintf("%s id=%d label=%s %s", p.Key, p.ID, p.Label, state))
	}
	return out
}

// TestPositionsFollowToolheadCount: a PrusaLink printer's positions are simply
// its toolheads, so id N is always toolhead:N labelled "Toolhead N", which is
// what its Spoolman locations and printed tags already say.
func TestPositionsFollowToolheadCount(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	b := newTestBridge(t, printer, spoolman)

	if err := b.SavePrinterConfig("printer_xl", PrinterConfig{Name: "XL", IPAddress: "127.0.0.1:2", APIKey: "k", Toolheads: 3}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"toolhead:0 id=0 label=Toolhead 0 present",
		"toolhead:1 id=1 label=Toolhead 1 present",
		"toolhead:2 id=2 label=Toolhead 2 present",
	}
	got := positionSummary(t, b, "printer_xl")
	if len(got) != len(want) {
		t.Fatalf("positions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d = %q, want %q", i, got[i], want[i])
		}
	}

	// Reconciling again changes nothing.
	if err := b.reconcileAllPositions(); err != nil {
		t.Fatal(err)
	}
	if again := positionSummary(t, b, "printer_xl"); len(again) != 3 {
		t.Errorf("reconcile is not idempotent: %v", again)
	}
}

// TestPositionsKeepIdsWhenCountShrinks: lowering the toolhead count must not
// renumber or delete anything. The extra positions go absent, and raising the
// count again brings back the same ids, so a mapping or a printed tag never
// starts meaning a different place.
func TestPositionsKeepIdsWhenCountShrinks(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	b := newTestBridge(t, printer, spoolman)

	cfg := PrinterConfig{Name: "XL", IPAddress: "127.0.0.1:2", APIKey: "k", Toolheads: 5}
	if err := b.SavePrinterConfig("printer_xl", cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Toolheads = 2
	if err := b.SavePrinterConfig("printer_xl", cfg); err != nil {
		t.Fatal(err)
	}

	positions, err := b.listPositions("printer_xl")
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 5 {
		t.Fatalf("shrinking the count deleted positions: %v", positionSummary(t, b, "printer_xl"))
	}
	for _, p := range positions {
		wantPresent := p.ID < 2
		if p.Present != wantPresent {
			t.Errorf("position %d present = %v, want %v", p.ID, p.Present, wantPresent)
		}
	}

	cfg.Toolheads = 5
	if err := b.SavePrinterConfig("printer_xl", cfg); err != nil {
		t.Fatal(err)
	}
	for _, p := range positionSummary(t, b, "printer_xl") {
		if strings.HasSuffix(p, "absent") {
			t.Errorf("raising the count left a position absent: %s", p)
		}
	}
}

// TestPositionsCoverReferencedIds: a mapping or a custom name can reference a
// toolhead beyond the current count. Those positions have to exist, or something
// in the database would point at a place FilaBridge cannot describe.
func TestPositionsCoverReferencedIds(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[6] = &fakeSpool{ID: 6, Name: "Stray", RemainingWeight: 400}
	b := newTestBridge(t, printer, spoolman)

	if err := b.SavePrinterConfig("printer_xl", PrinterConfig{Name: "XL", IPAddress: "127.0.0.1:2", APIKey: "k", Toolheads: 4}); err != nil {
		t.Fatal(err)
	}
	if err := b.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	if err := b.SetToolheadMapping("XL", 3, 6); err != nil {
		t.Fatal(err)
	}
	if err := b.SetToolheadName("printer_xl", 3, "Spare Head"); err != nil {
		t.Fatal(err)
	}

	// The printer loses two toolheads, but toolhead 3 is still referenced.
	if err := b.SavePrinterConfig("printer_xl", PrinterConfig{Name: "XL", IPAddress: "127.0.0.1:2", APIKey: "k", Toolheads: 2}); err != nil {
		t.Fatal(err)
	}
	p, ok := b.position("printer_xl", 3)
	if !ok {
		t.Fatal("a referenced position vanished when the count shrank")
	}
	if p.Present {
		t.Errorf("position 3 should be absent, got %+v", p)
	}
	if id, err := b.GetToolheadMapping("XL", 3); err != nil || id != 6 {
		t.Errorf("mapping on the absent position = %d (%v), want 6", id, err)
	}
}

// TestMappingWarningAcceptsSparsePositionIds: the answer to a mapping warning
// used to be validated by counting the offered slots, which assumed a slot's id
// was its place in the list. Ids are whatever the printer's positions are, so
// the check has to be membership.
func TestMappingWarningAcceptsSparsePositionIds(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	b := newTestBridge(t, printer, spoolman)

	if err := b.SavePrinterConfig("printer_xl", PrinterConfig{Name: "XL", IPAddress: "127.0.0.1:2", APIKey: "k", Toolheads: 3}); err != nil {
		t.Fatal(err)
	}
	if err := b.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	// A position whose id sits beyond the count, as a discovered layout produces.
	if err := b.migrateTx("test position", func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO printer_positions (printer_id, position_id, position_key, label, present) VALUES (?, ?, ?, ?, 1)`,
			"printer_xl", 9, "ext:255", "External Right")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	slots := b.buildMappingSlots("printer_xl", "XL", 3)
	var offered []int
	for _, s := range slots {
		offered = append(offered, s.ToolheadID)
	}
	if len(slots) != 4 || slots[len(slots)-1].ToolheadID != 9 {
		t.Fatalf("slots = %v, want the sparse position 9 offered last", offered)
	}
	if slots[len(slots)-1].DisplayName != "External Right" {
		t.Errorf("sparse slot label = %q, want its own label", slots[len(slots)-1].DisplayName)
	}

	b.warnMutex.Lock()
	b.mappingWarnings["w1"] = MappingWarning{
		ID: "w1", PrinterID: "printer_xl", PrinterName: "XL", ToolheadID: 0,
		JobID: 5, JobName: "part.bgcode", Grams: 20, Slots: slots,
	}
	b.warnMutex.Unlock()

	if _, err := b.AssignMappingWarningToolhead("w1", 9); err != nil {
		t.Errorf("answering with the sparse position was rejected: %v", err)
	}
	if _, err := b.AssignMappingWarningToolhead("w1", 7); err == nil {
		t.Error("answering with a position the printer does not have was accepted")
	}
}

// TestDeletingAPrinterTakesItsPositions: removing a printer leaves nothing of it
// behind, so adding one later starts clean rather than inheriting places that
// belonged to different hardware.
func TestDeletingAPrinterTakesItsPositions(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	b := newTestBridge(t, printer, spoolman)

	if err := b.SavePrinterConfig("printer_gone", PrinterConfig{Name: "Retired", IPAddress: "127.0.0.1:2", APIKey: "k", Toolheads: 3}); err != nil {
		t.Fatal(err)
	}
	if got := positionSummary(t, b, "printer_gone"); len(got) != 3 {
		t.Fatalf("setup: positions = %v", got)
	}
	if err := b.DeletePrinterConfig("printer_gone"); err != nil {
		t.Fatal(err)
	}
	if got := positionSummary(t, b, "printer_gone"); len(got) != 0 {
		t.Errorf("deleted printer left positions behind: %v", got)
	}
}

// TestAbsentPositionsStayPrinterLocations: a position the printer does not
// currently have is still a printer location. Its label is on printed tags and
// on spools in Spoolman, and if it read as ordinary storage a spool could record
// a toolhead as the place it lives and later be "returned" into it.
func TestAbsentPositionsStayPrinterLocations(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[5] = &fakeSpool{ID: 5, Name: "Blue", RemainingWeight: 700}
	b := newTestBridge(t, printer, spoolman)

	cfg := PrinterConfig{Name: "XL", IPAddress: "127.0.0.1:2", APIKey: "k", Toolheads: 3}
	if err := b.SavePrinterConfig("printer_xl", cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Toolheads = 1
	if err := b.SavePrinterConfig("printer_xl", cfg); err != nil {
		t.Fatal(err)
	}
	if err := b.ReloadConfig(); err != nil {
		t.Fatal(err)
	}

	if !b.toolheadLocationSet()["XL - Toolhead 2"] {
		t.Fatal("an absent toolhead's location reads as ordinary storage")
	}
	// So it is never recorded as a spool's home.
	b.rememberSpoolHome(5, "XL - Toolhead 2")
	if home := b.spoolHomeLocation(5); home != "" {
		t.Errorf("recorded a toolhead as spool 5's home: %q", home)
	}
}

// TestPrintHistoryRecordsPositionLabel: history says what the position was
// called when the print ran, so renaming a toolhead afterwards does not rewrite
// what already happened.
func TestPrintHistoryRecordsPositionLabel(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	b := newTestBridge(t, printer, spoolman)

	if err := b.SetToolheadName("printer_test", 0, "Left Head"); err != nil {
		t.Fatal(err)
	}
	if err := b.LogPrintUsage("TestPrinter", 0, 3, 12.5, "part.bgcode", time.Now(), "completed"); err != nil {
		t.Fatal(err)
	}
	if err := b.SetToolheadName("printer_test", 0, "Renamed Later"); err != nil {
		t.Fatal(err)
	}

	var label string
	if err := b.db.QueryRow(`SELECT position_label FROM print_history ORDER BY id DESC LIMIT 1`).Scan(&label); err != nil {
		t.Fatal(err)
	}
	if label != "Left Head" {
		t.Errorf("history label = %q, want the name it had at print time", label)
	}
}

// TestPositionsMigrationFromCountOnlyDatabase: a database written before
// positions existed gets them on open, matching what its toolhead counts and
// existing mappings already imply.
func TestPositionsMigrationFromCountOnlyDatabase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FILABRIDGE_DB_PATH", dir)

	db, err := sql.Open("sqlite", dir+"/"+DefaultDBFileName)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE printer_configs (printer_id TEXT PRIMARY KEY, name TEXT NOT NULL, ip_address TEXT NOT NULL, api_key TEXT, toolheads INTEGER DEFAULT 1, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO printer_configs (printer_id, name, ip_address, api_key, toolheads) VALUES ('p1', 'XL', '10.0.0.5', 'k', 5);
		INSERT INTO printer_configs (printer_id, name, ip_address, api_key, toolheads) VALUES ('p2', 'Mini', '10.0.0.6', 'k', 1);
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

	if got := positionSummary(t, bridge, "p1"); len(got) != 5 {
		t.Errorf("XL positions = %v, want 5", got)
	}
	if got := positionSummary(t, bridge, "p2"); len(got) != 1 || got[0] != "toolhead:0 id=0 label=Toolhead 0 present" {
		t.Errorf("Mini positions = %v", got)
	}
}
