package main

import (
	"strings"
	"testing"
	"time"
)

// testTime is a fixed moment, so payload comparisons do not depend on the clock.
func testTime() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

// x2dPositions are the places discovery allocates for one AMS and two holders.
func x2dPositions() []filamentPosition {
	return []filamentPosition{
		{ID: 0, Key: "ams:0:0", Label: "AMS A Slot 1", Present: true},
		{ID: 1, Key: "ams:0:1", Label: "AMS A Slot 2", Present: true},
		{ID: 2, Key: "ams:0:2", Label: "AMS A Slot 3", Present: true},
		{ID: 3, Key: "ams:0:3", Label: "AMS A Slot 4", Present: true},
		{ID: 4, Key: "ext:254", Label: "External Left", Present: true},
		{ID: 5, Key: "ext:255", Label: "External Right", Present: true},
	}
}

func x2dTestLayout() bambuLayout {
	layout, _ := parseBambuLayout([]byte(x2dLayoutReport(4, 1)))
	return layout
}

// TestAttributionFollowsThePrinter: each filament's grams land on the place the
// printer says fed it, whatever order the slicer listed them in.
func TestAttributionFollowsThePrinter(t *testing.T) {
	job := bambuJobRef{Mapping: []int{0, bambuExternalSpoolValue}, Layout: x2dTestLayout()}
	usage, err := bambuAttributeByPosition(map[int]float64{1: 94.36, 2: 0.84}, job, x2dPositions())
	if err != nil {
		t.Fatalf("attribution failed: %v", err)
	}
	if len(usage) != 2 || usage[0] != 94.36 || usage[5] != 0.84 {
		t.Fatalf("usage = %v, want 94.36 on AMS A Slot 1 and 0.84 on External Right", usage)
	}

	// The live X2D print that reported [65535, 65535, 2]: the unused project
	// slots decide nothing, and the grams go to AMS A Slot 3.
	job = bambuJobRef{Mapping: []int{65535, 65535, 2}, Layout: x2dTestLayout()}
	usage, err = bambuAttributeByPosition(map[int]float64{3: 11.4}, job, x2dPositions())
	if err != nil {
		t.Fatalf("unused slots broke attribution: %v", err)
	}
	if len(usage) != 1 || usage[2] != 11.4 {
		t.Errorf("usage = %v, want 11.4 on AMS A Slot 3 (id 2)", usage)
	}
}

// TestAttributionSumsSharedSources: one spool can print two of a project's
// filaments, and both lots of grams come off that spool.
func TestAttributionSumsSharedSources(t *testing.T) {
	job := bambuJobRef{Mapping: []int{2, 2}, Layout: x2dTestLayout()}
	usage, err := bambuAttributeByPosition(map[int]float64{1: 10.5, 2: 4.5}, job, x2dPositions())
	if err != nil {
		t.Fatalf("attribution failed: %v", err)
	}
	if len(usage) != 1 || usage[2] != 15.0 {
		t.Errorf("usage = %v, want both filaments summed as 15.0 on one position", usage)
	}
}

// TestAttributionRefusesToGuess: a source the printer never described, or one
// the printer has no position for, is an error naming the grams at stake.
// Recording them somewhere plausible is how a spool silently goes wrong.
func TestAttributionRefusesToGuess(t *testing.T) {
	cases := []struct {
		name       string
		byFilament map[int]float64
		job        bambuJobRef
		positions  []filamentPosition
		wantIn     string
	}{
		{
			name:       "source the printer never described",
			byFilament: map[int]float64{1: 12.25},
			job:        bambuJobRef{Mapping: []int{9999}, Layout: x2dTestLayout()},
			positions:  x2dPositions(),
			wantIn:     "12.25g was not recorded",
		},
		{
			name:       "mapping says nothing about this filament",
			byFilament: map[int]float64{2: 7.5},
			job:        bambuJobRef{Mapping: []int{0}, Layout: x2dTestLayout()},
			positions:  x2dPositions(),
			wantIn:     "7.50g was not recorded",
		},
		{
			name:       "printer has no position for a real source",
			byFilament: map[int]float64{1: 3.5},
			job:        bambuJobRef{Mapping: []int{2}, Layout: x2dTestLayout()},
			positions:  x2dPositions()[:1], // only AMS A Slot 1 allocated
			wantIn:     "3.50g was not recorded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usage, err := bambuAttributeByPosition(tc.byFilament, tc.job, tc.positions)
			if err == nil {
				t.Fatalf("guessed instead of refusing: %v", usage)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not say what was at stake (%q)", err, tc.wantIn)
			}
		})
	}
}

// TestAttributionWithoutMapping: an A1 never reports a mapping. With one place
// to load filament there is nothing to decide, and the tray the printer says is
// loaded answers a single-filament print on a machine with several.
func TestAttributionWithoutMapping(t *testing.T) {
	a1, _ := parseBambuLayout(loadCapture(t, "a1_push_status.json"))
	single := []filamentPosition{{ID: 0, Key: "ext:254", Label: "External", Present: true}}

	usage, err := bambuAttributeByPosition(map[int]float64{1: 51.53}, bambuJobRef{Layout: a1}, single)
	if err != nil {
		t.Fatalf("a printer with one position should need no mapping: %v", err)
	}
	if len(usage) != 1 || usage[0] != 51.53 {
		t.Errorf("usage = %v, want all of it on the only position", usage)
	}

	// Several places and no mapping: the loaded tray decides a single-filament
	// print.
	job := bambuJobRef{Layout: x2dTestLayout(), TrayNow: 2}
	usage, err = bambuAttributeByPosition(map[int]float64{1: 11.4}, job, x2dPositions())
	if err != nil {
		t.Fatalf("the loaded tray should have answered this: %v", err)
	}
	if len(usage) != 1 || usage[2] != 11.4 {
		t.Errorf("usage = %v, want 11.4 on the loaded AMS A Slot 3", usage)
	}

	// Several places, no mapping and more than one filament: not knowable.
	job = bambuJobRef{Layout: x2dTestLayout(), TrayNow: 2}
	if _, err := bambuAttributeByPosition(map[int]float64{1: 10, 2: 5}, job, x2dPositions()); err == nil {
		t.Error("two filaments with no mapping must not be guessed at")
	}
}

// TestBambuHistoryUsesTheJobName: an X2D reports the same internal path as
// gcode_file for every print, so history recorded under the filename reads as
// one job repeated forever. It is recorded under the name the printer gives the
// job instead, and falls back to the filename only when the printer names none.
func TestBambuHistoryUsesTheJobName(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[23] = &fakeSpool{ID: 23, Name: "ASA", RemainingWeight: 900}
	b := newTestBridge(t, printer, spoolman)
	config, _ := bambuTestPrinter(t, b, bambuStateFinish, "")
	if err := b.SetToolheadMapping("X1C", 0, 23); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		jobID   int
		jobName string
		want    string
	}{
		{101, "ASA_swatch.stl", "ASA_swatch.stl"},
		{102, "", "/data/Metadata/plate_1.gcode"},
	} {
		active := &activeJob{
			PrinterID: "printer_bambu", JobID: tc.jobID,
			Filename: "/data/Metadata/plate_1.gcode", JobName: tc.jobName,
			StartedAt: testTime(), LastProgress: 1, Usage: map[int]float64{0: 6.02},
		}

		// The name has to survive being stored mid-print and read back at the end.
		if err := b.upsertActiveJob(active); err != nil {
			t.Fatal(err)
		}
		stored, err := b.getActiveJob("printer_bambu")
		if err != nil || stored == nil || stored.JobName != tc.jobName {
			t.Fatalf("stored job name = %+v (%v), want %q", stored, err, tc.jobName)
		}

		if err := b.handleBambuPrintEnded(config, stored, 1, true); err != nil {
			t.Fatalf("recording job %d: %v", tc.jobID, err)
		}
		history, err := b.GetPrintHistory(1)
		if err != nil || len(history) == 0 {
			t.Fatalf("no history row for job %d: %v", tc.jobID, err)
		}
		if history[0].JobName != tc.want {
			t.Errorf("history job name = %q, want %q", history[0].JobName, tc.want)
		}
	}
}

// TestRunoutNotificationNamesThePosition: webhook consumers keep the toolhead_id
// they already read, and a numbered toolhead keeps its exact wording. A place
// with a name of its own says that instead, since "toolhead 5" means nothing on
// a machine whose places are AMS slots.
func TestRunoutNotificationNamesThePosition(t *testing.T) {
	prusa := lowFilamentPayload(RunoutWarning{
		PrinterName: "XL", ToolheadID: 4, PositionKey: "toolhead:4", PositionLabel: "Toolhead 4",
		SpoolID: 14, SpoolName: "Black", RequiredWeight: 80, RemainingWeight: 20,
	}, testTime())
	if !strings.Contains(prusa.Message, "on XL toolhead 4 is short") {
		t.Errorf("PrusaLink wording changed: %q", prusa.Message)
	}
	if prusa.ToolheadID == nil || *prusa.ToolheadID != 4 {
		t.Errorf("toolhead_id = %v, want 4 for consumers already reading it", prusa.ToolheadID)
	}

	bambu := lowFilamentPayload(RunoutWarning{
		PrinterName: "X2D", ToolheadID: 2, PositionKey: "ams:0:2", PositionLabel: "AMS A Slot 3",
		SpoolID: 18, SpoolName: "White", RequiredWeight: 80, RemainingWeight: 20,
	}, testTime())
	if !strings.Contains(bambu.Message, "on X2D AMS A Slot 3 is short") {
		t.Errorf("message does not name the position: %q", bambu.Message)
	}
	if bambu.PositionLabel != "AMS A Slot 3" {
		t.Errorf("position_label = %q, want the position's name", bambu.PositionLabel)
	}
	if bambu.ToolheadID == nil || *bambu.ToolheadID != 2 {
		t.Errorf("toolhead_id = %v, want it kept for existing consumers", bambu.ToolheadID)
	}
}
