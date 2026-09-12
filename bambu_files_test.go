package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

// projectFilament describes one filament in a fixture project file.
type projectFilament struct {
	ID       int
	Material string
	Grams    float64
}

// makeProject3MF builds a .3mf shaped like the ones Bambu Studio writes: the
// project title, the plate's gcode header, and slice_info's per-filament grams.
// Those three are what identify a file, so the fixtures carry all of them.
func makeProject3MF(t *testing.T, title string, layers int, filaments ...projectFilament) []byte {
	t.Helper()
	var slice strings.Builder
	slice.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<config>\n  <plate>\n    <metadata key=\"index\" value=\"1\"/>\n")
	for _, f := range filaments {
		fmt.Fprintf(&slice, `    <filament id="%d" type="%s" color="#FFFFFF" used_g="%.2f"/>`+"\n", f.ID, f.Material, f.Grams)
	}
	slice.WriteString("  </plate>\n</config>")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write(sliceInfoPath, slice.String())
	write(bambuTitlePath, fmt.Sprintf(`<?xml version="1.0"?><model><metadata name="Title">%s</metadata></model>`, title))
	write(fmt.Sprintf(bambuPlateGcode, 1), fmt.Sprintf("; HEADER_BLOCK_START\n; total layer number: %d\n; total filament weight [g] : 11.40\nG1 X0 Y0\n", layers))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// benchyDrive mirrors the real drive this was built against: four files sliced
// from one project, so all four share a title, two Benchy slices at 240 layers
// and two Swatch slices at 14, each pair differing only by filament.
func benchyDrive(t *testing.T) map[string][]byte {
	t.Helper()
	const title = "Benchy Optimised For All Filaments "
	return map[string][]byte{
		"Benchy_PETG_X2D-04.gcode.3mf": makeProject3MF(t, title, 240, projectFilament{1, "PETG", 11.40}),
		"Benchy_PLA_X2D-04.gcode.3mf":  makeProject3MF(t, title, 240, projectFilament{1, "PLA", 11.48}),
		"Swatch_PETG_X2D-04.gcode.3mf": makeProject3MF(t, title, 14, projectFilament{1, "PETG", 1.20}),
		"Swatch_PLA_X2D-04.gcode.3mf":  makeProject3MF(t, title, 14, projectFilament{1, "PLA", 1.25}),
	}
}

// x2dJob is a print started at the printer from a file already on the drive: the
// reported name is the project's title, not a filename, and gcode_file points at
// an internal path FTPS cannot reach.
func x2dJob(layers int, material string) bambuJobRef {
	return bambuJobRef{
		GcodeFile:   "/data/Metadata/plate_1.gcode",
		SubtaskName: "Benchy Optimised For All Filaments ",
		PlateIdx:    1,
		Mapping:     []int{2},
		Layers:      layers,
		Sources:     map[int]string{2: material},
	}
}

// TestBambuFindsFileByContent: with four files sharing one title, the file is
// identified by the layer count and the material loaded in the tray the printer
// says is feeding the print. Names decide nothing, so a reprint of a file
// uploaded long ago resolves exactly like a fresh one.
func TestBambuFindsFileByContent(t *testing.T) {
	srv := newFakeBambuFTPS(t, benchyDrive(t))

	for _, tc := range []struct {
		name     string
		layers   int
		material string
		want     string
		grams    float64
	}{
		{"benchy in PETG", 240, "PETG", "Benchy_PETG_X2D-04.gcode.3mf", 11.40},
		{"benchy in PLA", 240, "PLA", "Benchy_PLA_X2D-04.gcode.3mf", 11.48},
		{"swatch in PLA", 14, "PLA", "Swatch_PLA_X2D-04.gcode.3mf", 1.25},
		{"swatch in PETG", 14, "PETG", "Swatch_PETG_X2D-04.gcode.3mf", 1.20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := bambuFindSlicedFile(srv.host, srv.port(), "accesscode", x2dJob(tc.layers, tc.material), newBambuFileIndex())
			if err != nil {
				t.Fatalf("no file identified: %v", err)
			}
			if file.Path != tc.want {
				t.Errorf("identified %q, want %q", file.Path, tc.want)
			}
			if file.Usage[1] != tc.grams {
				t.Errorf("grams = %v, want %v", file.Usage, tc.grams)
			}
		})
	}
}

// TestBambuNameHintIsVerified: Bambu Studio reuses cached project names, so the
// file sitting at the expected name can be an older slice of the same project.
// The hint must never be trusted on its own, or a print would be recorded
// against another file's grams.
func TestBambuNameHintIsVerified(t *testing.T) {
	files := benchyDrive(t)
	// A stale upload under the name this job's title would produce, left over
	// from an earlier slice at a different layer height.
	files["cache/Benchy Optimised For All Filaments .gcode.3mf"] =
		makeProject3MF(t, "Benchy Optimised For All Filaments ", 118, projectFilament{1, "PETG", 47.10})
	srv := newFakeBambuFTPS(t, files)

	file, err := bambuFindSlicedFile(srv.host, srv.port(), "accesscode", x2dJob(240, "PETG"), newBambuFileIndex())
	if err != nil {
		t.Fatalf("no file identified: %v", err)
	}
	if file.Path != "Benchy_PETG_X2D-04.gcode.3mf" {
		t.Fatalf("identified %q, want the file whose layer count matches the printer", file.Path)
	}
	if file.Usage[1] != 11.40 {
		t.Errorf("recorded the stale file's grams: %v", file.Usage)
	}
}

// TestBambuFreshUploadUsesHint: a job Studio just sent is found by name without
// reading the rest of the drive.
func TestBambuFreshUploadUsesHint(t *testing.T) {
	files := benchyDrive(t)
	files["cache/Hotends_Box.gcode.3mf"] = makeProject3MF(t, "Hotends Box", 125,
		projectFilament{1, "PLA", 0.93}, projectFilament{2, "PLA", 103.52})
	srv := newFakeBambuFTPS(t, files)

	job := bambuJobRef{
		GcodeFile:   "/data/Metadata/plate_1.gcode",
		SubtaskName: "Hotends_Box",
		PlateIdx:    1,
		Mapping:     []int{0, bambuExternalSpool},
		Layers:      125,
		Sources:     map[int]string{0: "PLA", bambuExternalSpool: "PLA"},
	}
	file, err := bambuFindSlicedFile(srv.host, srv.port(), "accesscode", job, newBambuFileIndex())
	if err != nil {
		t.Fatalf("fresh upload not found: %v", err)
	}
	if file.Path != "cache/Hotends_Box.gcode.3mf" {
		t.Fatalf("identified %q, want the cached upload", file.Path)
	}
	if srv.downloads() != 1 {
		t.Errorf("read %d files, want 1: the name hint should avoid scanning the drive", srv.downloads())
	}
}

// TestBambuRefusesAmbiguousFiles: when the printer's facts cannot separate two
// files that disagree on grams, nothing is recorded and the error names them.
// Copies of one slice agree on grams, so those are used rather than refused.
func TestBambuRefusesAmbiguousFiles(t *testing.T) {
	const title = "Twins"
	job := bambuJobRef{
		GcodeFile: "/data/Metadata/plate_1.gcode", SubtaskName: title, PlateIdx: 1,
		Mapping: []int{2}, Layers: 90, Sources: map[int]string{2: "PLA"},
	}

	conflicting := newFakeBambuFTPS(t, map[string][]byte{
		"one.gcode.3mf": makeProject3MF(t, title, 90, projectFilament{1, "PLA", 20.0}),
		"two.gcode.3mf": makeProject3MF(t, title, 90, projectFilament{1, "PLA", 31.5}),
	})
	_, err := bambuFindSlicedFile(conflicting.host, conflicting.port(), "accesscode", job, newBambuFileIndex())
	if err == nil {
		t.Fatal("two files disagreeing on grams must not be guessed between")
	}
	for _, want := range []string{"one.gcode.3mf", "two.gcode.3mf"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}

	duplicates := newFakeBambuFTPS(t, map[string][]byte{
		"one.gcode.3mf":      makeProject3MF(t, title, 90, projectFilament{1, "PLA", 20.0}),
		"one copy.gcode.3mf": makeProject3MF(t, title, 90, projectFilament{1, "PLA", 20.0}),
	})
	file, err := bambuFindSlicedFile(duplicates.host, duplicates.port(), "accesscode", job, newBambuFileIndex())
	if err != nil {
		t.Fatalf("identical copies should be interchangeable: %v", err)
	}
	if file.Usage[1] != 20.0 {
		t.Errorf("grams = %v, want 20", file.Usage)
	}
}

// TestBambuNoMatchingFile: an internal-storage print has no file on the drive at
// all, which has to surface as an error rather than the nearest guess.
func TestBambuNoMatchingFile(t *testing.T) {
	srv := newFakeBambuFTPS(t, benchyDrive(t))
	// A job whose layer count matches nothing on the drive.
	_, err := bambuFindSlicedFile(srv.host, srv.port(), "accesscode", x2dJob(999, "PLA"), newBambuFileIndex())
	if err == nil {
		t.Fatal("a print with no file on the drive must not resolve to one")
	}
}

// TestBambuFileIndexAvoidsRepeatReads: identifying the same print twice reads the
// drive once, so a reprint of an old file is not a fresh download every time.
func TestBambuFileIndexAvoidsRepeatReads(t *testing.T) {
	srv := newFakeBambuFTPS(t, benchyDrive(t))
	idx := newBambuFileIndex()
	job := x2dJob(240, "PETG")

	if _, err := bambuFindSlicedFile(srv.host, srv.port(), "accesscode", job, idx); err != nil {
		t.Fatal(err)
	}
	first := srv.downloads()
	if first == 0 {
		t.Fatal("expected the first lookup to read the drive")
	}
	if _, err := bambuFindSlicedFile(srv.host, srv.port(), "accesscode", job, idx); err != nil {
		t.Fatal(err)
	}
	if srv.downloads() != first {
		t.Errorf("second lookup read %d more files, want 0", srv.downloads()-first)
	}
}

// TestParseBambuSources reads the filament blocks from reports captured off a
// live A1 and X2D, so the lookup keys stay pinned to what the printers send.
func TestParseBambuSources(t *testing.T) {
	a1, err := os.ReadFile("testdata/bambu/a1_push_status.json")
	if err != nil {
		t.Fatal(err)
	}
	// The A1 has no AMS and feeds from the external spool, which it reports as
	// vt_tray id 254.
	if got := parseBambuSources(a1); got[254] != "PLA" {
		t.Errorf("A1 external spool = %v, want PLA at key 254 (all: %v)", got[254], got)
	}

	x2d, err := os.ReadFile("testdata/bambu/x2d_push_status.json")
	if err != nil {
		t.Fatal(err)
	}
	got := parseBambuSources(x2d)
	// AMS unit 0 tray 2 held the PETG this print was running from, reachable by
	// the flat tray number and by the unit<<8|slot form the X2D uses.
	if got[2] != "PETG" {
		t.Errorf("AMS tray 2 = %q, want PETG (all: %v)", got[2], got)
	}
	// The external spool holder, reported as vir_slot 255 with black PLA.
	if got[bambuExternalSpool] != "PLA" {
		t.Errorf("external spool = %q, want PLA at %d", got[bambuExternalSpool], bambuExternalSpool)
	}
	// A delta report carries no filament blocks, and must leave the last answer
	// standing rather than clearing it.
	if parseBambuSources([]byte(`{"print":{"mc_percent":40,"command":"push_status","msg":1}}`)) != nil {
		t.Error("a delta report must not produce a source map")
	}
}
