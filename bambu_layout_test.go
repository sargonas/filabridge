package main

import (
	"os"
	"strings"
	"testing"
)

func loadCapture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/bambu/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestBambuLayoutFromX2D reads the layout out of a report captured from a live
// X2D: one AMS of four trays, two external holders, two nozzles.
func TestBambuLayoutFromX2D(t *testing.T) {
	layout, complete := parseBambuLayout(loadCapture(t, "x2d_push_status.json"))
	if !complete {
		t.Error("a full state push should be trusted as complete")
	}
	if len(layout.Units) != 1 || len(layout.Units[0].Trays) != 4 {
		t.Fatalf("AMS units = %+v, want one unit of four trays", layout.Units)
	}
	if layout.Units[0].ID != 0 {
		t.Errorf("AMS unit id = %d, want 0", layout.Units[0].ID)
	}
	// Tray 2 held the PETG this print was running from.
	if got := layout.Units[0].Trays[2]; got.ID != 2 || got.Material != "PETG" {
		t.Errorf("tray 2 = %+v, want id 2 holding PETG", got)
	}
	if layout.Nozzles != 2 {
		t.Errorf("nozzles = %d, want 2", layout.Nozzles)
	}

	var extIDs []int
	for _, e := range layout.Externals {
		extIDs = append(extIDs, e.ID)
	}
	if len(extIDs) != 2 || extIDs[0] != 254 || extIDs[1] != 255 {
		t.Errorf("external holders = %v, want [254 255]", extIDs)
	}
}

// TestBambuLayoutFromA1: a bare A1 has no AMS and reports its single external
// holder as vt_tray 254, with no device block at all.
func TestBambuLayoutFromA1(t *testing.T) {
	layout, complete := parseBambuLayout(loadCapture(t, "a1_push_status.json"))
	if !complete {
		t.Error("the A1's full push should be trusted as complete")
	}
	if len(layout.Units) != 0 {
		t.Errorf("AMS units = %+v, want none", layout.Units)
	}
	if len(layout.Externals) != 1 || layout.Externals[0].ID != 254 {
		t.Fatalf("externals = %+v, want one holder with id 254", layout.Externals)
	}
	if layout.Externals[0].Material != "PLA" {
		t.Errorf("external material = %q, want PLA", layout.Externals[0].Material)
	}
	if layout.Nozzles != 1 {
		t.Errorf("nozzles = %d, want 1 when the report has no device block", layout.Nozzles)
	}
}

// TestBambuLayoutIgnoresDeltas: the small reports an A1 sends between full
// pushes carry no filament blocks. Treating one as complete would read as a
// printer that had just lost its AMS.
func TestBambuLayoutIgnoresDeltas(t *testing.T) {
	delta := []byte(`{"print":{"bed_temper":64.625,"command":"push_status","msg":1,"sequence_id":"2643"}}`)
	layout, complete := parseBambuLayout(delta)
	if complete {
		t.Error("a delta report must never be treated as a complete layout")
	}
	if !layout.Empty() {
		t.Errorf("delta produced a layout: %+v", layout)
	}

	// Nor may a malformed payload.
	if _, complete := parseBambuLayout([]byte(`{"print":`)); complete {
		t.Error("unparsable payload reported as complete")
	}
}

// TestBambuLayoutLenientTypes: ids arrive as strings on real printers and as
// numbers in places, and unloaded trays carry no material at all.
func TestBambuLayoutLenientTypes(t *testing.T) {
	payload := []byte(`{"print":{"command":"push_status","msg":0,
		"ams":{"ams":[{"id":1,"tray":[{"id":"0","tray_type":"PLA"},{"id":1,"state":0}]}]},
		"vt_tray":{"id":"254","tray_type":""}}}`)
	layout, complete := parseBambuLayout(payload)
	if !complete {
		t.Fatal("expected a complete layout")
	}
	if len(layout.Units) != 1 || layout.Units[0].ID != 1 {
		t.Fatalf("units = %+v", layout.Units)
	}
	if layout.Units[0].Trays[0].Material != "PLA" || layout.Units[0].Trays[1].Material != "" {
		t.Errorf("trays = %+v, want the second one empty", layout.Units[0].Trays)
	}
	if len(layout.Externals) != 1 || layout.Externals[0].ID != 254 {
		t.Errorf("externals = %+v", layout.Externals)
	}
}

// TestDecodeBambuSource pins the wire encoding. Values marked live were captured
// from a real printer; the rest are the encoding applied consistently.
func TestDecodeBambuSource(t *testing.T) {
	cases := []struct {
		value int
		want  string // "" means refused
		note  string
	}{
		{0, "ams:0:0", "live: X2D printing from AMS tray 0"},
		{2, "ams:0:2", "live: X2D printing from AMS tray 2"},
		{3, "ams:0:3", ""},
		{65280, "ext:255", "live: X2D external holder (0xFF00)"},
		{65024, "ext:254", "0xFE00, the other holder"},
		{254, "ext:254", "live: single nozzle vt_tray"},
		{255, "ext:255", ""},
		{256, "ams:1:0", "unit<<8|slot, a second AMS"},
		{259, "ams:1:3", ""},
		{4, "ams:1:0", "flat numbering on older multi-AMS printers"},
		{65535, "", "an unused project filament slot"},
		{-1, "", "never valid"},
	}

	// A layout with everything the cases refer to.
	layout := bambuLayout{
		Units: []bambuAMSUnit{
			{ID: 0, Trays: []bambuTray{{ID: 0}, {ID: 1}, {ID: 2}, {ID: 3}}},
			{ID: 1, Trays: []bambuTray{{ID: 0}, {ID: 1}, {ID: 2}, {ID: 3}}},
		},
		Externals: []bambuExternalSlot{{ID: 254}, {ID: 255}},
		Nozzles:   2,
	}
	for _, tc := range cases {
		got, ok := decodeBambuSource(tc.value, layout)
		if tc.want == "" {
			if ok {
				t.Errorf("decode(%d) = %q, want refused (%s)", tc.value, got, tc.note)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("decode(%d) = %q (ok %v), want %q (%s)", tc.value, got, ok, tc.want, tc.note)
		}
	}

	// A value that decodes but names hardware this printer does not have is
	// refused rather than invented: an A1 has no AMS at all.
	a1, _ := parseBambuLayout(loadCapture(t, "a1_push_status.json"))
	if got, ok := decodeBambuSource(2, a1); ok {
		t.Errorf("decode(2) on a printer with no AMS = %q, want refused", got)
	}
	if got, ok := decodeBambuSource(254, a1); !ok || got != "ext:254" {
		t.Errorf("decode(254) on the A1 = %q (ok %v), want ext:254", got, ok)
	}
}

// TestBambuPositionLabels: labels follow how Bambu itself names things, units
// lettered from A and slots counted from one. They must never contain " - ",
// which separates the printer's name from the position in a Spoolman location
// and on every printed tag.
func TestBambuPositionLabels(t *testing.T) {
	dual := bambuLayout{Nozzles: 2}
	single := bambuLayout{Nozzles: 1}
	cases := []struct {
		key    string
		layout bambuLayout
		want   string
	}{
		{"ams:0:0", dual, "AMS A Slot 1"},
		{"ams:0:3", dual, "AMS A Slot 4"},
		{"ams:1:2", dual, "AMS B Slot 3"},
		{"ams:128:0", dual, "AMS HT 1 Slot 1"},
		{"ext:255", dual, "External Right"},
		{"ext:254", dual, "External Left"},
		{"ext:254", single, "External"},
		{"ext:255", single, "External"},
	}
	for _, tc := range cases {
		got := bambuPositionLabel(tc.key, tc.layout)
		if got != tc.want {
			t.Errorf("label(%s) = %q, want %q", tc.key, got, tc.want)
		}
		if strings.Contains(got, " - ") {
			t.Errorf("label(%s) = %q contains the location separator", tc.key, got)
		}
	}
}

// TestBambuLayoutPositionOrder: positions are listed AMS first by unit and slot,
// then the external holders, which is the order the dashboard shows them in.
func TestBambuLayoutPositionOrder(t *testing.T) {
	layout, _ := parseBambuLayout(loadCapture(t, "x2d_push_status.json"))
	want := []string{"ams:0:0", "ams:0:1", "ams:0:2", "ams:0:3", "ext:254", "ext:255"}
	got := bambuLayoutPositions(layout)
	if len(got) != len(want) {
		t.Fatalf("positions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d = %q, want %q", i, got[i], want[i])
		}
	}
}
