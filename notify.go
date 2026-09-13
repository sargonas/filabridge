package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// notificationTimeout bounds how long a webhook POST may take. Notifications are
// best-effort and fired from goroutines, so a slow or dead endpoint must never
// hold a monitoring goroutine open for long.
const notificationTimeout = 10 * time.Second

// NotificationPayload is the JSON body POSTed to the configured webhook. It
// carries a human-readable title/message (for simple targets like ntfy, Gotify,
// or a Discord/Slack relay) alongside structured fields (for automation targets
// like Home Assistant or n8n). Event-specific fields are omitted when empty.
type NotificationPayload struct {
	Event     string    `json:"event"` // "low_filament" | "printer_offline" | "unknown_filament_slot"
	Title     string    `json:"title"`
	Message   string    `json:"message"`
	Printer   string    `json:"printer"`
	Timestamp time.Time `json:"timestamp"`

	// low_filament
	SpoolID   int    `json:"spool_id,omitempty"`
	SpoolName string `json:"spool_name,omitempty"`
	// ToolheadID stays for the consumers already reading it. PositionLabel is
	// what the place is called ("AMS A Slot 3"), which a number cannot say.
	ToolheadID       *int    `json:"toolhead_id,omitempty"`
	PositionLabel    string  `json:"position_label,omitempty"`
	RequiredWeightG  float64 `json:"required_weight_g,omitempty"`
	RemainingWeightG float64 `json:"remaining_weight_g,omitempty"`
	AutoPaused       bool    `json:"auto_paused,omitempty"`

	// printer_offline
	LastState string `json:"last_state,omitempty"`

	// unknown_filament_slot
	Filename          string  `json:"filename,omitempty"`
	UnrecordedWeightG float64 `json:"unrecorded_weight_g,omitempty"`
}

// isActivePrintState reports whether a printer state means a print was in
// progress (running, paused mid-job, or awaiting attention) rather than idle or
// finished. Losing connection in one of these states is unexpected and worth a
// notification; losing it while idle/finished is a normal power-off.
func isActivePrintState(state string) bool {
	return state == StatePrinting || state == StatePaused || state == StateAttention
}

// lowFilamentPayload builds the notification for a low-filament warning, noting
// when the print was auto-paused as a result.
// positionPhrase names a place in a sentence. A numbered toolhead keeps the
// wording it has always had, since these messages go to webhooks people have
// already built around. Anywhere else reads better as its own name: "AMS A Slot
// 3" says more than "toolhead 6".
func positionPhrase(positionKey, label string, toolheadID int) string {
	if label == "" || strings.HasPrefix(positionKey, positionKeyToolhead+":") || positionKey == "" {
		return fmt.Sprintf("toolhead %d", toolheadID)
	}
	return label
}

func lowFilamentPayload(w RunoutWarning, at time.Time) NotificationPayload {
	shortage := w.RequiredWeight - w.RemainingWeight
	var msg strings.Builder
	fmt.Fprintf(&msg, "Spool \"%s\" (ID %d) on %s %s is short by %.1fg: %.1fg remaining, print needs ~%.1fg.",
		w.SpoolName, w.SpoolID, w.PrinterName, positionPhrase(w.PositionKey, w.PositionLabel, w.ToolheadID),
		shortage, w.RemainingWeight, w.RequiredWeight)

	title := fmt.Sprintf("Low filament on %s", w.PrinterName)
	if w.AutoPaused {
		title = fmt.Sprintf("Print auto-paused on %s (low filament)", w.PrinterName)
		msg.WriteString(" The print has been paused; swap the spool, then acknowledge the warning in FilaBridge (or resume at the printer).")
	}

	toolheadID := w.ToolheadID
	return NotificationPayload{
		Event:            "low_filament",
		Title:            title,
		Message:          msg.String(),
		Printer:          w.PrinterName,
		Timestamp:        at,
		SpoolID:          w.SpoolID,
		SpoolName:        w.SpoolName,
		ToolheadID:       &toolheadID,
		PositionLabel:    w.PositionLabel,
		RequiredWeightG:  w.RequiredWeight,
		RemainingWeightG: w.RemainingWeight,
		AutoPaused:       w.AutoPaused,
	}
}

// printerOfflinePayload builds the notification for an unexpected loss of
// connection to a printer that was mid-print.
func printerOfflinePayload(printerName, lastState string, at time.Time) NotificationPayload {
	return NotificationPayload{
		Event:     "printer_offline",
		Title:     fmt.Sprintf("Printer offline: %s", printerName),
		Message:   fmt.Sprintf("Lost connection to %s during an active print (last state: %s). The print may have been interrupted.", printerName, lastState),
		Printer:   printerName,
		Timestamp: at,
		LastState: lastState,
	}
}

// mappingWarningPayload builds the notification for a single-filament print on a
// multi-toolhead printer. The slice records no slot, so the usage is attributed
// to the given toolhead by default. It fires at print start rather than at
// completion precisely so the right toolhead can still be picked while the print
// runs, which is the difference between debiting the right spool and the wrong
// one.
func mappingWarningPayload(printerName, filename string, toolheadID int, grams float64, at time.Time) NotificationPayload {
	msg := fmt.Sprintf("%s is printing ~%.1fg from a single-filament slice, which does not record which slot it used. FilaBridge will record it against toolhead %d. Confirm that is the toolhead it is really printing from, or pick the right one on the dashboard before the print finishes.",
		printerName, grams, toolheadID)

	return NotificationPayload{
		Event:             "unknown_filament_slot",
		Title:             fmt.Sprintf("Confirm toolhead mapping on %s", printerName),
		Message:           msg,
		Printer:           printerName,
		Timestamp:         at,
		ToolheadID:        &toolheadID,
		Filename:          filename,
		UnrecordedWeightG: grams,
	}
}

// notifyInBackground sends a notification without holding up the caller, and
// registers the send so Close waits for it. An untracked send can still be
// reading the webhook setting from the database while it closes.
func (b *FilamentBridge) notifyInBackground(p NotificationPayload) {
	b.background.Add(1)
	go func() {
		defer b.background.Done()
		b.sendNotification(p)
	}()
}

// sendNotification POSTs the payload to the configured webhook URL. It no-ops
// when no URL is configured, and is best-effort: delivery failures are logged,
// never propagated. Callers go through notifyInBackground so a slow endpoint
// never blocks the monitoring loop.
func (b *FilamentBridge) sendNotification(p NotificationPayload) {
	url, err := b.GetConfigValue(ConfigKeyNotifyWebhookURL)
	if err != nil || strings.TrimSpace(url) == "" {
		return // notifications disabled
	}
	url = strings.TrimSpace(url)

	body, err := json.Marshal(p)
	if err != nil {
		log.Printf("Warning: could not marshal %s notification: %v", p.Event, err)
		return
	}

	client := &http.Client{Timeout: notificationTimeout}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("Warning: %s notification webhook POST failed: %v", p.Event, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("Warning: %s notification webhook returned HTTP %d", p.Event, resp.StatusCode)
	}
}
