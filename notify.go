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
	Event     string    `json:"event"` // "low_filament" | "printer_offline"
	Title     string    `json:"title"`
	Message   string    `json:"message"`
	Printer   string    `json:"printer"`
	Timestamp time.Time `json:"timestamp"`

	// low_filament
	SpoolID          int     `json:"spool_id,omitempty"`
	SpoolName        string  `json:"spool_name,omitempty"`
	ToolheadID       *int    `json:"toolhead_id,omitempty"`
	RequiredWeightG  float64 `json:"required_weight_g,omitempty"`
	RemainingWeightG float64 `json:"remaining_weight_g,omitempty"`
	AutoPaused       bool    `json:"auto_paused,omitempty"`

	// printer_offline
	LastState string `json:"last_state,omitempty"`
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
func lowFilamentPayload(w RunoutWarning, at time.Time) NotificationPayload {
	shortage := w.RequiredWeight - w.RemainingWeight
	var msg strings.Builder
	fmt.Fprintf(&msg, "Spool \"%s\" (ID %d) on %s toolhead %d is short by %.1fg: %.1fg remaining, print needs ~%.1fg.",
		w.SpoolName, w.SpoolID, w.PrinterName, w.ToolheadID, shortage, w.RemainingWeight, w.RequiredWeight)

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

// sendNotification POSTs the payload to the configured webhook URL. It no-ops
// when no URL is configured, and is best-effort: delivery failures are logged,
// never propagated. Callers invoke it from a goroutine so a slow endpoint never
// blocks the monitoring loop.
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
