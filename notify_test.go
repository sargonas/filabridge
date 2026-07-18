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
