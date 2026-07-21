package main

import (
	"archive/zip"
	"bytes"
	"testing"
	"time"
)

// makeThreeMF wraps slice_info XML in a minimal .3mf (zip) for parser tests.
func makeThreeMF(t *testing.T, sliceInfoXML string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(sliceInfoPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(sliceInfoXML)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestParseSliceInfoUsage uses the exact slice_info.config captured from a live
// A1 print, so the parser stays pinned to the real Bambu format.
func TestParseSliceInfoUsage(t *testing.T) {
	const realXML = `<?xml version="1.0" encoding="UTF-8"?>
<config>
  <header>
    <header_item key="X-BBL-Client-Type" value="slicer"/>
  </header>
  <plate>
    <metadata key="index" value="1"/>
    <metadata key="weight" value="24.15"/>
    <object identify_id="453" name="Grid 4x5.stl" skipped="false" />
    <filament id="1" tray_info_idx="GFSNL04" type="PLA" color="#00AE42" used_m="8.30" used_g="24.15" group_id="0"/>
  </plate>
</config>`
	usage, err := parseSliceInfoUsage(makeThreeMF(t, realXML), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[1] != 24.15 {
		t.Fatalf("want map[1:24.15], got %v", usage)
	}
}

// TestParseSliceInfoUsageMultiPlate covers plate selection and skipping unused
// (0 g) filament slots on a multi-material, multi-plate file.
func TestParseSliceInfoUsageMultiPlate(t *testing.T) {
	const xml = `<config>
  <plate>
    <metadata key="index" value="1"/>
    <filament id="1" type="PLA" used_g="10.0"/>
  </plate>
  <plate>
    <metadata key="index" value="2"/>
    <filament id="1" type="PLA" used_g="5.5"/>
    <filament id="2" type="PETG" used_g="0.00"/>
    <filament id="3" type="ABS" used_g="7.25"/>
  </plate>
</config>`

	plate2, err := parseSliceInfoUsage(makeThreeMF(t, xml), 2)
	if err != nil {
		t.Fatal(err)
	}
	if plate2[1] != 5.5 || plate2[3] != 7.25 {
		t.Fatalf("plate 2 grams wrong: %v", plate2)
	}
	if _, ok := plate2[2]; ok {
		t.Errorf("unused 0 g filament must be skipped: %v", plate2)
	}

	// plateIndex 0 selects the first plate.
	first, err := parseSliceInfoUsage(makeThreeMF(t, xml), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[1] != 10.0 {
		t.Fatalf("default plate wrong: %v", first)
	}
}

// fakeMQTTMessage is a minimal mqtt.Message carrying only a payload, for
// exercising onMessage without a broker.
type fakeMQTTMessage struct{ payload []byte }

func (m fakeMQTTMessage) Duplicate() bool   { return false }
func (m fakeMQTTMessage) Qos() byte         { return 0 }
func (m fakeMQTTMessage) Retained() bool    { return false }
func (m fakeMQTTMessage) Topic() string     { return "" }
func (m fakeMQTTMessage) MessageID() uint16 { return 0 }
func (m fakeMQTTMessage) Payload() []byte   { return m.payload }
func (m fakeMQTTMessage) Ack()              {}

// TestBambuReportPartialMerge verifies the core assumption behind the state
// cache: Bambu emits partial reports, and unmarshaling each into the same struct
// must update only the fields present, leaving the rest intact.
func TestBambuReportPartialMerge(t *testing.T) {
	bc := &bambuClient{serial: "TEST"}

	bc.onMessage(nil, fakeMQTTMessage{[]byte(`{"print":{"gcode_state":"RUNNING","subtask_name":"benchy","gcode_file":"benchy.3mf","mc_percent":10}}`)})
	got, ok := bc.snapshot()
	if !ok {
		t.Fatal("expected haveReport true after first message")
	}
	if got.Print.GcodeState != bambuStateRunning || got.Print.SubtaskName != "benchy" || got.Print.McPercent != 10 {
		t.Fatalf("initial report not parsed: %+v", got.Print)
	}

	// Partial update: only progress. State/name/file must survive.
	bc.onMessage(nil, fakeMQTTMessage{[]byte(`{"print":{"mc_percent":55}}`)})
	got, _ = bc.snapshot()
	if got.Print.McPercent != 55 {
		t.Errorf("mc_percent not updated: %d", got.Print.McPercent)
	}
	if got.Print.GcodeState != bambuStateRunning || got.Print.SubtaskName != "benchy" || got.Print.GcodeFile != "benchy.3mf" {
		t.Errorf("partial update clobbered prior fields: %+v", got.Print)
	}

	// Terminal transition keeps the job name for end-of-print handling.
	bc.onMessage(nil, fakeMQTTMessage{[]byte(`{"print":{"gcode_state":"FINISH","mc_percent":100}}`)})
	got, _ = bc.snapshot()
	if got.Print.GcodeState != bambuStateFinish || got.Print.McPercent != 100 || got.Print.SubtaskName != "benchy" {
		t.Errorf("terminal update wrong: %+v", got.Print)
	}
}

func TestBambuToToolheadUsage(t *testing.T) {
	// slice_info filament ids are 1-based; toolheads are 0-based.
	got := bambuToToolheadUsage(map[int]float64{1: 24.15, 3: 7.0})
	if got[0] != 24.15 || got[2] != 7.0 {
		t.Fatalf("1-based -> 0-based mapping wrong: %v", got)
	}
	if _, ok := got[1]; ok {
		t.Errorf("filament id 1 should map to toolhead 0, not 1: %v", got)
	}
}

func TestBambuJobID(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	a := bambuJobID("benchy.gcode.3mf", start)
	b := bambuJobID("benchy.gcode.3mf", start)
	if a != b {
		t.Errorf("job id must be stable for same inputs: %d != %d", a, b)
	}
	if a == 0 {
		t.Error("job id must never be 0 (the no-dedupe sentinel)")
	}
	if bambuJobID("benchy.gcode.3mf", start.Add(time.Second)) == a {
		t.Error("different start time should yield a different job id")
	}
	if bambuJobID("other.gcode.3mf", start) == a {
		t.Error("different filename should yield a different job id")
	}
}

func TestBambuStatePredicates(t *testing.T) {
	for _, s := range []string{bambuStateRunning, bambuStatePause, bambuStatePrepare} {
		if !bambuStateIsPrinting(s) {
			t.Errorf("%s should be printing", s)
		}
		if bambuStateIsTerminal(s) {
			t.Errorf("%s should not be terminal", s)
		}
	}
	for _, s := range []string{bambuStateFinish, bambuStateFailed, bambuStateIdle} {
		if !bambuStateIsTerminal(s) {
			t.Errorf("%s should be terminal", s)
		}
		if bambuStateIsPrinting(s) {
			t.Errorf("%s should not be printing", s)
		}
	}
}

func TestBambuJobName(t *testing.T) {
	if n := bambuJobName(bambuReport{}); n != "No active job" {
		t.Errorf("empty report name = %q", n)
	}
	var r bambuReport
	r.Print.GcodeFile = "file.3mf"
	if n := bambuJobName(r); n != "file.3mf" {
		t.Errorf("filename fallback = %q", n)
	}
	r.Print.SubtaskName = "My Print"
	if n := bambuJobName(r); n != "My Print" {
		t.Errorf("subtask-name preference = %q", n)
	}
}
