package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// webhookCapture is a test webhook target that records received notification
// payloads on a channel.
func webhookCapture(t *testing.T) (*httptest.Server, chan NotificationPayload) {
	t.Helper()
	ch := make(chan NotificationPayload, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p NotificationPayload
		json.NewDecoder(r.Body).Decode(&p)
		ch <- p
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

// expectExactlyOne asserts a single notification is delivered and no second one
// arrives shortly after — the anti-spam guarantee for edge-triggered events.
func expectExactlyOne(t *testing.T, ch chan NotificationPayload) NotificationPayload {
	t.Helper()
	var got NotificationPayload
	select {
	case got = <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("expected one notification, got none")
	}
	select {
	case extra := <-ch:
		t.Fatalf("expected exactly one notification, got a second: %+v", extra)
	case <-time.After(300 * time.Millisecond):
	}
	return got
}

func TestIsActivePrintState(t *testing.T) {
	for _, s := range []string{StatePrinting, StatePaused, StateAttention} {
		if !isActivePrintState(s) {
			t.Errorf("state %q should count as an active print", s)
		}
	}
	for _, s := range []string{StateIdle, StateFinished, StateStopped, StateError, StateOffline, ""} {
		if isActivePrintState(s) {
			t.Errorf("state %q should not count as an active print", s)
		}
	}
}

func TestLowFilamentPayload(t *testing.T) {
	w := RunoutWarning{
		PrinterName:     "CoreOne",
		ToolheadID:      1,
		SpoolID:         7,
		SpoolName:       "Galaxy Black",
		RequiredWeight:  120,
		RemainingWeight: 80,
	}

	p := lowFilamentPayload(w, time.Unix(0, 0))
	if p.Event != "low_filament" || p.Printer != "CoreOne" {
		t.Fatalf("unexpected payload: %+v", p)
	}
	if p.ToolheadID == nil || *p.ToolheadID != 1 {
		t.Errorf("toolhead id = %v, want 1", p.ToolheadID)
	}
	if p.RequiredWeightG != 120 || p.RemainingWeightG != 80 {
		t.Errorf("weights wrong: %+v", p)
	}
	if p.AutoPaused || strings.Contains(strings.ToLower(p.Message), "paus") {
		t.Errorf("non-paused warning must not mention pausing: %q", p.Message)
	}

	// Auto-paused variant surfaces the pause in both title and message.
	w.AutoPaused = true
	p = lowFilamentPayload(w, time.Unix(0, 0))
	if !p.AutoPaused {
		t.Fatal("AutoPaused not carried into payload")
	}
	if !strings.Contains(p.Title, "auto-paused") || !strings.Contains(strings.ToLower(p.Message), "paused") {
		t.Errorf("auto-paused payload should mention pausing: title=%q msg=%q", p.Title, p.Message)
	}
}

func TestSendNotificationGatingAndDelivery(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	b := newTestBridge(t, printer, spoolman)
	srv, ch := webhookCapture(t)

	// No webhook configured: must not deliver.
	b.sendNotification(printerOfflinePayload("P", StatePrinting, time.Unix(0, 0)))
	select {
	case p := <-ch:
		t.Fatalf("delivered with no webhook configured: %+v", p)
	case <-time.After(200 * time.Millisecond):
	}

	// With a webhook configured, it delivers the payload verbatim.
	if err := b.SetConfigValue(ConfigKeyNotifyWebhookURL, srv.URL); err != nil {
		t.Fatal(err)
	}
	b.sendNotification(printerOfflinePayload("CoreOne", StatePrinting, time.Unix(0, 0)))
	select {
	case p := <-ch:
		if p.Event != "printer_offline" || p.Printer != "CoreOne" || p.LastState != StatePrinting {
			t.Errorf("wrong payload delivered: %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook was not called")
	}
}

// TestLowFilamentNotificationFires drives a real low-filament warning through
// the monitor cycle and asserts the webhook receives a low_filament payload,
// including the auto_paused flag when pause mode is enabled.
func TestLowFilamentNotificationFires(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[3] = &fakeSpool{ID: 3, Name: "Nearly Empty", RemainingWeight: 40}
	b := newTestBridge(t, printer, spoolman)
	b.SetToolheadMapping("TestPrinter", 0, 3)
	b.SetConfigValue(ConfigKeyRunoutPauseEnabled, "true")
	srv, ch := webhookCapture(t)
	if err := b.SetConfigValue(ConfigKeyNotifyWebhookURL, srv.URL); err != nil {
		t.Fatal(err)
	}

	printer.set(func(f *fakePrusaLink) {
		f.State = "PRINTING"
		f.JobID = 48
		f.Filename = "part.bgcode"
		f.FileBody = bgcodeFixture("372.68", 1024)
	})
	cycle(t, b)

	select {
	case p := <-ch:
		if p.Event != "low_filament" {
			t.Errorf("event = %q, want low_filament", p.Event)
		}
		if !p.AutoPaused {
			t.Error("auto_paused = false, want true (pause mode enabled)")
		}
		if p.SpoolID != 3 || p.RemainingWeightG != 40 {
			t.Errorf("payload spool fields wrong: %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("low-filament notification not delivered")
	}
}

// TestOfflineNotificationOnlyDuringActivePrint: a printer dropping while idle is
// a normal power-off (no notification); dropping mid-print is unexpected and
// must notify.
func TestOfflineNotificationOnlyDuringActivePrint(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	b := newTestBridge(t, printer, spoolman)
	srv, ch := webhookCapture(t)
	if err := b.SetConfigValue(ConfigKeyNotifyWebhookURL, srv.URL); err != nil {
		t.Fatal(err)
	}
	offline := errors.New("connection refused")

	// Idle -> offline: no notification.
	b.noteStateChange("printer_test", "TestPrinter", StateIdle, "")
	b.noteConnectivity("printer_test", "1.2.3.4", "TestPrinter", offline)
	select {
	case p := <-ch:
		t.Fatalf("idle->offline should not notify: %+v", p)
	case <-time.After(300 * time.Millisecond):
	}

	// Clear the offline edge so the next drop is a fresh transition.
	b.noteConnectivity("printer_test", "1.2.3.4", "TestPrinter", nil)

	// Printing -> offline: must notify with the last active state.
	b.noteStateChange("printer_test", "TestPrinter", StatePrinting, "job.gcode")
	b.noteConnectivity("printer_test", "1.2.3.4", "TestPrinter", offline)
	select {
	case p := <-ch:
		if p.Event != "printer_offline" || p.LastState != StatePrinting {
			t.Errorf("wrong offline payload: %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("printing->offline did not notify")
	}
}

// TestLowFilamentNotifiesOncePerWarning: repeated monitor passes over the same
// job raise the warning once, so exactly one notification is sent — no per-poll
// spam.
func TestLowFilamentNotifiesOncePerWarning(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[3] = &fakeSpool{ID: 3, Name: "Nearly Empty", RemainingWeight: 40}
	b := newTestBridge(t, printer, spoolman)
	b.SetToolheadMapping("TestPrinter", 0, 3)
	srv, ch := webhookCapture(t)
	if err := b.SetConfigValue(ConfigKeyNotifyWebhookURL, srv.URL); err != nil {
		t.Fatal(err)
	}

	printer.set(func(f *fakePrusaLink) {
		f.State = "PRINTING"
		f.JobID = 51
		f.Filename = "part.bgcode"
		f.FileBody = bgcodeFixture("372.68", 1024)
	})
	cycle(t, b)
	cycle(t, b)
	cycle(t, b)

	if p := expectExactlyOne(t, ch); p.Event != "low_filament" {
		t.Errorf("event = %q, want low_filament", p.Event)
	}
}

// TestOfflineNotifiesOncePerDrop: an offline printer is observed on every poll,
// but the notification is edge-triggered — one alert per drop, not per poll.
func TestOfflineNotifiesOncePerDrop(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	b := newTestBridge(t, printer, spoolman)
	srv, ch := webhookCapture(t)
	if err := b.SetConfigValue(ConfigKeyNotifyWebhookURL, srv.URL); err != nil {
		t.Fatal(err)
	}
	offline := errors.New("connection refused")

	b.noteStateChange("printer_test", "TestPrinter", StatePrinting, "job.gcode")
	b.noteConnectivity("printer_test", "1.2.3.4", "TestPrinter", offline)
	b.noteConnectivity("printer_test", "1.2.3.4", "TestPrinter", offline)
	b.noteConnectivity("printer_test", "1.2.3.4", "TestPrinter", offline)

	if p := expectExactlyOne(t, ch); p.Event != "printer_offline" {
		t.Errorf("event = %q, want printer_offline", p.Event)
	}
}

// TestPayloadSerializesToolheadZero guards the *int/omitempty choice: toolhead 0
// must appear in the JSON. A plain int with omitempty would silently drop it.
func TestPayloadSerializesToolheadZero(t *testing.T) {
	w := RunoutWarning{PrinterName: "P", ToolheadID: 0, SpoolID: 1, SpoolName: "S", RequiredWeight: 10, RemainingWeight: 5}
	body, err := json.Marshal(lowFilamentPayload(w, time.Unix(0, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"toolhead_id":0`) {
		t.Errorf("toolhead 0 must serialize; omitempty must not drop it. got: %s", body)
	}
}

// TestSendNotificationHandlesFailuresCleanly: a webhook that errors (HTTP 500 or
// an unreachable host) must be swallowed — logged, never panicking or blocking.
func TestSendNotificationHandlesFailuresCleanly(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	b := newTestBridge(t, printer, spoolman)
	payload := printerOfflinePayload("P", StatePrinting, time.Unix(0, 0))

	// Endpoint returns HTTP 500.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := b.SetConfigValue(ConfigKeyNotifyWebhookURL, srv.URL); err != nil {
		t.Fatal(err)
	}
	b.sendNotification(payload) // must return cleanly, no panic

	// Nothing listening: connection refused, must return promptly without panic.
	if err := b.SetConfigValue(ConfigKeyNotifyWebhookURL, "http://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { b.sendNotification(payload); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sendNotification hung on an unreachable endpoint")
	}
}

// TestMappingWarningPayload covers the single-filament warning's content.
func TestMappingWarningPayload(t *testing.T) {
	p := mappingWarningPayload("CoreOne", "part.bgcode", 0, 102.48, time.Unix(0, 0))
	if p.Event != "unknown_filament_slot" || p.Printer != "CoreOne" {
		t.Fatalf("unexpected payload: %+v", p)
	}
	if p.ToolheadID == nil || *p.ToolheadID != 0 {
		t.Errorf("toolhead id = %v, want 0", p.ToolheadID)
	}
	if p.UnrecordedWeightG != 102.48 || p.Filename != "part.bgcode" {
		t.Errorf("payload fields wrong: %+v", p)
	}
	// The point of firing at print start is that there is still time to act.
	if !strings.Contains(strings.ToLower(p.Message), "before the print finishes") {
		t.Errorf("message should say the mapping can still be fixed: %q", p.Message)
	}
	if !strings.Contains(strings.ToLower(p.Message), "single-filament") {
		t.Errorf("message should name the cause: %q", p.Message)
	}
}

// TestUnknownSlotNotificationRespectsToggle: silence when the setting is off,
// and on a multi-filament slice, which names its own slots.
func TestUnknownSlotNotificationRespectsToggle(t *testing.T) {
	startPrinting := func(f *fakePrusaLink) {
		f.State = "PRINTING"
		f.JobID = 92
		f.Filename = "part.bgcode"
		f.FileBody = bgcodeFixture("102.48", 1024)
	}

	t.Run("multi-filament slice stays quiet", func(t *testing.T) {
		printer := newFakePrusaLink(t)
		spoolman := newFakeSpoolman(t)
		b := multiToolheadTestBridge(t, printer, spoolman, 5)
		srv, ch := webhookCapture(t)
		b.SetConfigValue(ConfigKeyNotifyWebhookURL, srv.URL)

		printer.set(func(f *fakePrusaLink) {
			startPrinting(f)
			// Five filaments, so the slice named its own slots.
			f.FileBody = bgcodeFixture("6.12,6.20,6.21,4.03,3.78", 1024)
		})
		cycle(t, b)

		select {
		case p := <-ch:
			t.Fatalf("multi-filament slice must not warn: %+v", p)
		case <-time.After(500 * time.Millisecond):
		}
	})

	t.Run("setting off stays quiet", func(t *testing.T) {
		printer := newFakePrusaLink(t)
		spoolman := newFakeSpoolman(t)
		b := multiToolheadTestBridge(t, printer, spoolman, 5)
		srv, ch := webhookCapture(t)
		b.SetConfigValue(ConfigKeyNotifyWebhookURL, srv.URL)
		if err := b.SetConfigValue(ConfigKeyNotifyUnknownSlot, "false"); err != nil {
			t.Fatal(err)
		}

		printer.set(startPrinting)
		cycle(t, b)

		select {
		case p := <-ch:
			t.Fatalf("notified with the setting off: %+v", p)
		case <-time.After(500 * time.Millisecond):
		}
	})
}

// multiToolheadTestBridge reconfigures the standard test printer as a
// multi-toolhead machine (MMU/INDX style), where a single-filament slice's
// missing slot actually matters.
func multiToolheadTestBridge(t *testing.T, printer *fakePrusaLink, spoolman *fakeSpoolman, toolheads int) *FilamentBridge {
	t.Helper()
	b := newTestBridge(t, printer, spoolman)
	if err := b.SavePrinterConfig("printer_test", PrinterConfig{
		Name:      "TestPrinter",
		IPAddress: printer.Addr(),
		APIKey:    "test-key",
		Toolheads: toolheads,
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return b
}

// TestUnknownSlotWarnsEvenWhenToolheadIsMapped is the case that silently debits
// the wrong spool: a single-filament slice names no slot, so its usage is
// attributed to toolhead 0, and on a multi-toolhead printer with toolhead 0
// mapped that deduction looks perfectly successful while being wrong.
func TestUnknownSlotWarnsEvenWhenToolheadIsMapped(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[9] = &fakeSpool{ID: 9, Name: "Slot 1 spool", RemainingWeight: 900}
	b := multiToolheadTestBridge(t, printer, spoolman, 5)
	if err := b.SetToolheadMapping("TestPrinter", 0, 9); err != nil {
		t.Fatal(err)
	}
	srv, ch := webhookCapture(t)
	if err := b.SetConfigValue(ConfigKeyNotifyWebhookURL, srv.URL); err != nil {
		t.Fatal(err)
	}

	printer.set(func(f *fakePrusaLink) {
		f.State = "PRINTING"
		f.JobID = 93
		f.Filename = "single.bgcode"
		f.FileBody = bgcodeFixture("102.48", 1024)
	})
	cycle(t, b)

	p := expectExactlyOne(t, ch)
	if p.Event != "unknown_filament_slot" {
		t.Fatalf("event = %q, want unknown_filament_slot", p.Event)
	}
	if !strings.Contains(strings.ToLower(p.Message), "confirm") {
		t.Errorf("expected a confirm-the-mapping warning, got: %q", p.Message)
	}

	warnings := b.GetMappingWarnings()
	if len(warnings) != 1 {
		t.Fatalf("expected 1 dashboard warning, got %d", len(warnings))
	}
	if warnings[0].ToolheadID != 0 || warnings[0].Grams != 102.48 {
		t.Errorf("warning fields wrong: %+v", warnings[0])
	}

	// Acknowledging removes it from the dashboard.
	if err := b.AcknowledgeMappingWarning(warnings[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := b.GetMappingWarnings(); len(got) != 0 {
		t.Errorf("acknowledged warning still listed: %+v", got)
	}
}

// TestSingleToolheadPrinterNeverWarnsOnUnknownSlot: with one toolhead there is
// nowhere else the filament could have come from, so the attribution is always
// right and saying anything would be noise.
func TestSingleToolheadPrinterNeverWarnsOnUnknownSlot(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[9] = &fakeSpool{ID: 9, Name: "The only spool", RemainingWeight: 900}
	b := newTestBridge(t, printer, spoolman) // Toolheads: 1
	if err := b.SetToolheadMapping("TestPrinter", 0, 9); err != nil {
		t.Fatal(err)
	}
	srv, ch := webhookCapture(t)
	b.SetConfigValue(ConfigKeyNotifyWebhookURL, srv.URL)

	printer.set(func(f *fakePrusaLink) {
		f.State = "PRINTING"
		f.JobID = 94
		f.Filename = "single.bgcode"
		f.FileBody = bgcodeFixture("102.48", 1024)
	})
	cycle(t, b)

	select {
	case p := <-ch:
		t.Fatalf("single-toolhead printer must not warn: %+v", p)
	case <-time.After(500 * time.Millisecond):
	}
	if got := b.GetMappingWarnings(); len(got) != 0 {
		t.Errorf("single-toolhead printer raised a warning: %+v", got)
	}
}
