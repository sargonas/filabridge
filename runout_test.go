package main

import (
	"fmt"
	"testing"
)

// TestRunoutWarningInformational: default configuration warns without pausing,
// acknowledging dismisses, and the warning never re-fires for the same job.
func TestRunoutWarningInformational(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[3] = &fakeSpool{ID: 3, Name: "Nearly Empty", RemainingWeight: 40, UsedWeight: 960}
	bridge := newTestBridge(t, printer, spoolman)
	bridge.SetToolheadMapping("TestPrinter", 0, 3)

	printer.set(func(f *fakePrusaLink) {
		f.State = "PRINTING"
		f.JobID = 46
		f.Filename = "part.bgcode"
		f.FileBody = bgcodeFixture("372.68", 1024)
	})
	cycle(t, bridge)

	warnings := bridge.GetRunoutWarnings()
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(warnings))
	}
	w := warnings[0]
	if w.RequiredWeight != 372.68 || w.RemainingWeight != 40 || w.AutoPaused {
		t.Errorf("warning fields: %+v", w)
	}
	if printer.PauseCalls != 0 {
		t.Error("informational mode must not pause")
	}

	if err := bridge.AcknowledgeRunoutWarning(w.ID); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	cycle(t, bridge)
	cycle(t, bridge)
	if len(bridge.GetRunoutWarnings()) != 0 {
		t.Error("warning re-fired after acknowledge")
	}
	if printer.ResumeCalls != 0 {
		t.Error("informational acknowledge must not resume")
	}
}

// TestRunoutPauseAndResume: pause mode pauses the print; acknowledging resumes
// it; a print the user already resumed is left alone.
func TestRunoutPauseAndResume(t *testing.T) {
	for _, userResumedFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("userResumedFirst=%v", userResumedFirst), func(t *testing.T) {
			printer := newFakePrusaLink(t)
			spoolman := newFakeSpoolman(t)
			spoolman.Spools[3] = &fakeSpool{ID: 3, Name: "Nearly Empty", RemainingWeight: 40}
			bridge := newTestBridge(t, printer, spoolman)
			bridge.SetToolheadMapping("TestPrinter", 0, 3)
			bridge.SetConfigValue(ConfigKeyRunoutPauseEnabled, "true")

			printer.set(func(f *fakePrusaLink) {
				f.State = "PRINTING"
				f.JobID = 47
				f.Filename = "part.bgcode"
				f.FileBody = bgcodeFixture("372.68", 1024)
			})
			cycle(t, bridge)

			if printer.PauseCalls != 1 {
				t.Fatalf("pause calls = %d, want 1", printer.PauseCalls)
			}
			if printer.State != "PAUSED" {
				t.Fatalf("printer state = %s, want PAUSED", printer.State)
			}
			warnings := bridge.GetRunoutWarnings()
			if len(warnings) != 1 || !warnings[0].AutoPaused {
				t.Fatalf("warnings: %+v", warnings)
			}

			if userResumedFirst {
				printer.set(func(f *fakePrusaLink) { f.State = "PRINTING" })
			}

			if err := bridge.AcknowledgeRunoutWarning(warnings[0].ID); err != nil {
				t.Fatalf("acknowledge: %v", err)
			}
			if printer.State != "PRINTING" {
				t.Errorf("printer state after acknowledge = %s, want PRINTING", printer.State)
			}
			wantResumes := 1
			if userResumedFirst {
				wantResumes = 0 // already printing: no resume call
			}
			if printer.ResumeCalls != wantResumes {
				t.Errorf("resume calls = %d, want %d", printer.ResumeCalls, wantResumes)
			}
		})
	}
}

// TestRunoutWarningClearsWhenJobEnds: an unacknowledged warning (user resumed
// at the printer and ignored the dashboard) must not survive the print, and
// recording must happen regardless.
func TestRunoutWarningClearsWhenJobEnds(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[3] = &fakeSpool{ID: 3, Name: "Nearly Empty", RemainingWeight: 40}
	bridge := newTestBridge(t, printer, spoolman)
	bridge.SetToolheadMapping("TestPrinter", 0, 3)
	bridge.SetConfigValue(ConfigKeyRunoutPauseEnabled, "true")

	printer.set(func(f *fakePrusaLink) {
		f.State = "PRINTING"
		f.JobID = 48
		f.Filename = "part.bgcode"
		f.FileBody = bgcodeFixture("372.68", 1024)
	})
	cycle(t, bridge)
	if len(bridge.GetRunoutWarnings()) != 1 {
		t.Fatal("warning did not fire")
	}

	// User resumes at the printer, never acknowledges
	printer.set(func(f *fakePrusaLink) { f.State = "PRINTING" })
	cycle(t, bridge)

	printer.set(func(f *fakePrusaLink) { f.State = "FINISHED" })
	cycle(t, bridge)

	if len(bridge.GetRunoutWarnings()) != 0 {
		t.Error("warning outlived its print")
	}
	history, _ := bridge.GetPrintHistory(10)
	if len(history) != 1 || history[0].FilamentUsed != 372.68 {
		t.Errorf("recording must be independent of warnings: %+v", history)
	}
}

// TestRunoutSufficientSpoolStaysSilent covers both the plain sufficient case
// and the remaining-fraction math (a mostly-done print needs less than the
// full estimate).
func TestRunoutSufficientSpoolStaysSilent(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[3] = &fakeSpool{ID: 3, Name: "Nearly Empty", RemainingWeight: 40}
	bridge := newTestBridge(t, printer, spoolman)
	bridge.SetToolheadMapping("TestPrinter", 0, 3)

	// 90% done: needs only 37.268g of the 372.68g estimate; 40g suffices
	printer.set(func(f *fakePrusaLink) {
		f.State = "PRINTING"
		f.JobID = 49
		f.Progress = 90
		f.Filename = "part.bgcode"
		f.FileBody = bgcodeFixture("372.68", 1024)
	})
	cycle(t, bridge)

	if warnings := bridge.GetRunoutWarnings(); len(warnings) != 0 {
		t.Errorf("fraction math failed, got warning: %+v", warnings)
	}
}

// TestRunoutDisabled verifies the master toggle silences the feature entirely,
// including the pause toggle.
func TestRunoutDisabled(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[3] = &fakeSpool{ID: 3, Name: "Nearly Empty", RemainingWeight: 40}
	bridge := newTestBridge(t, printer, spoolman)
	bridge.SetToolheadMapping("TestPrinter", 0, 3)
	bridge.SetConfigValue(ConfigKeyRunoutWarningEnabled, "false")
	bridge.SetConfigValue(ConfigKeyRunoutPauseEnabled, "true")

	printer.set(func(f *fakePrusaLink) {
		f.State = "PRINTING"
		f.JobID = 50
		f.Filename = "part.bgcode"
		f.FileBody = bgcodeFixture("372.68", 1024)
	})
	cycle(t, bridge)

	if len(bridge.GetRunoutWarnings()) != 0 || printer.PauseCalls != 0 {
		t.Error("disabled feature still acted")
	}
}
