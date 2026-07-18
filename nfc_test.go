package main

import "testing"

// TestParseLocationParamResolvesToolhead covers the toolhead-location parser
// after the findPrinterByName refactor: "PrinterName - X" must resolve to the
// right toolhead whether X is a custom name or the default "Toolhead N", must
// reject out-of-range numeric IDs and unknown printers, and must treat a bare
// name as a non-printer storage location.
func TestParseLocationParamResolvesToolhead(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	b := newTestBridge(t, printer, spoolman)

	// Give the single toolhead a custom display name.
	if err := b.SetToolheadName("printer_test", 0, "LeftNozzle"); err != nil {
		t.Fatalf("SetToolheadName: %v", err)
	}

	cases := []struct {
		name     string
		location string
		wantTH   int
		wantIsTH bool
	}{
		{"custom name", "TestPrinter - LeftNozzle", 0, true},
		{"default name still resolves", "TestPrinter - Toolhead 0", 0, true},
		{"numeric out of range", "TestPrinter - Toolhead 5", 0, false},
		{"unknown printer", "GhostPrinter - Toolhead 0", 0, false},
		{"bare storage location", "Drybox", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, th, _, isTH, err := b.parseLocationParam(tc.location)
			if err != nil {
				t.Fatalf("parseLocationParam(%q): %v", tc.location, err)
			}
			if isTH != tc.wantIsTH {
				t.Fatalf("parseLocationParam(%q) isPrinterLocation = %v, want %v", tc.location, isTH, tc.wantIsTH)
			}
			if isTH && th != tc.wantTH {
				t.Fatalf("parseLocationParam(%q) toolheadID = %d, want %d", tc.location, th, tc.wantTH)
			}
		})
	}
}
