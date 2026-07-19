package main

import (
	"strings"
	"testing"
)

// TestImportMappingsFromSpoolman covers rebuilding a printer's toolhead mappings
// from Spoolman spool locations: a clean import, a location claimed by two spools
// (conflict, skipped), a toolhead with no spool (unmatched, reported), moving a
// spool off a stale FilaBridge mapping, and ignoring other printers' locations.
func TestImportMappingsFromSpoolman(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	b := newTestBridge(t, printer, spoolman)

	// A managed 3-toolhead printer to import.
	if err := b.SavePrinterConfig("printer_multi", PrinterConfig{Name: "Multi", IPAddress: "127.0.0.1:2", APIKey: "k", Toolheads: 3}); err != nil {
		t.Fatal(err)
	}
	if err := b.ReloadConfig(); err != nil {
		t.Fatal(err)
	}

	for _, id := range []int{1, 2, 3, 4, 5} {
		spoolman.Spools[id] = &fakeSpool{ID: id, Name: "Spool", RemainingWeight: 500}
	}

	// Pre-map spool 1 to TestPrinter/0 in FilaBridge (stale), then move it to
	// Multi/0 in Spoolman — the import should follow Spoolman and vacate TestPrinter.
	if err := b.SetToolheadMapping("TestPrinter", 0, 1); err != nil {
		t.Fatalf("pre-map: %v", err)
	}
	spoolman.Spools[1].Location = "Multi - Toolhead 0"       // clean import
	spoolman.Spools[2].Location = "Multi - Toolhead 1"       // conflict...
	spoolman.Spools[3].Location = "Multi - Toolhead 1"       // ...with spool 2
	spoolman.Spools[4].Location = "TestPrinter - Toolhead 0" // another printer's location
	spoolman.Spools[5].Location = "Ghost - Toolhead 0"       // unmanaged printer
	// Multi - Toolhead 2 intentionally has no spool -> unmatched

	summary, err := b.ImportMappingsFromSpoolman("Multi")
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if summary.Imported != 1 {
		t.Errorf("imported = %d, want 1", summary.Imported)
	}
	if len(summary.Conflicts) != 1 || !strings.Contains(summary.Conflicts[0], "Multi - Toolhead 1") {
		t.Errorf("conflicts = %v, want one for Multi - Toolhead 1", summary.Conflicts)
	}
	if len(summary.Unmatched) != 1 || summary.Unmatched[0] != "Multi - Toolhead 2" {
		t.Errorf("unmatched = %v, want [Multi - Toolhead 2]", summary.Unmatched)
	}

	if id, _ := b.GetToolheadMapping("Multi", 0); id != 1 {
		t.Errorf("Multi/0 mapping = %d, want 1", id)
	}
	if id, _ := b.GetToolheadMapping("Multi", 1); id != 0 {
		t.Errorf("Multi/1 (conflict) mapping = %d, want unmapped", id)
	}
	if id, _ := b.GetToolheadMapping("Multi", 2); id != 0 {
		t.Errorf("Multi/2 (unmatched) mapping = %d, want unmapped", id)
	}
	// Spool 1 moved to Multi/0, so its stale TestPrinter/0 mapping is gone.
	if id, _ := b.GetToolheadMapping("TestPrinter", 0); id != 0 {
		t.Errorf("TestPrinter/0 = %d, want vacated (spool moved to Multi)", id)
	}
}
