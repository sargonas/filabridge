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

type AppriseNotifier struct {
	httpClient *http.Client
}

func NewAppriseNotifier(timeoutSeconds int) *AppriseNotifier {
	return &AppriseNotifier{
		httpClient: &http.Client{
			Timeout: time.Duration(timeoutSeconds) * time.Second,
		},
	}
}

type appriseStatelessRequest struct {
	URLs  []string `json:"urls"`
	Title string   `json:"title"`
	Body  string   `json:"body"`
	Type  string   `json:"type"`
}

type appriseStatefulRequest struct {
	Tag   string `json:"tag"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Type  string `json:"type"`
}

func (n *AppriseNotifier) Send(apiURL string, urls []string, title, body, notifType string) error {
	endpoint := strings.TrimRight(apiURL, "/") + "/notify/"

	payload := appriseStatelessRequest{
		URLs:  urls,
		Title: title,
		Body:  body,
		Type:  notifType,
	}
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal notification: %w", err)
	}

	resp, err := n.httpClient.Post(endpoint, "application/json", bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to send notification: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("apprise API returned status %d", resp.StatusCode)
	}
	return nil
}

func (n *AppriseNotifier) SendStateful(apiURL, key, tag, title, body, notifType string) error {
	endpoint := strings.TrimRight(apiURL, "/") + "/notify/" + key

	payload := appriseStatefulRequest{
		Tag:   tag,
		Title: title,
		Body:  body,
		Type:  notifType,
	}
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal notification: %w", err)
	}

	resp, err := n.httpClient.Post(endpoint, "application/json", bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to send notification: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("apprise API returned status %d", resp.StatusCode)
	}
	return nil
}

func (n *AppriseNotifier) TestConnection(apiURL string) error {
	endpoint := strings.TrimRight(apiURL, "/") + "/status"

	resp, err := n.httpClient.Get(endpoint)
	if err != nil {
		return fmt.Errorf("failed to connect to Apprise API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("apprise API returned status %d", resp.StatusCode)
	}
	return nil
}

// notify fires an asynchronous notification if Apprise is enabled and the
// specific event toggle is on. Failures are logged but never block the caller.
func (b *FilamentBridge) notify(eventKey, title, body, notifType string) {
	enabled, err := b.GetConfigValue(ConfigKeyAppriseEnabled)
	if err != nil || enabled != "true" {
		return
	}

	eventEnabled, err := b.GetConfigValue(eventKey)
	if err != nil || eventEnabled == "false" {
		return
	}

	apiURL, err := b.GetConfigValue(ConfigKeyAppriseAPIURL)
	if err != nil || apiURL == "" {
		return
	}

	mode, _ := b.GetConfigValue(ConfigKeyAppriseMode)

	if mode == "stateful" {
		key, _ := b.GetConfigValue(ConfigKeyAppriseKey)
		tag, _ := b.GetConfigValue(ConfigKeyAppriseTag)
		if key == "" {
			return
		}
		go func() {
			if err := b.notifier.SendStateful(apiURL, key, tag, title, body, notifType); err != nil {
				log.Printf("Warning: failed to send %s notification: %v", eventKey, err)
			}
		}()
		return
	}

	rawURLs, err := b.GetConfigValue(ConfigKeyAppriseURLs)
	if err != nil || rawURLs == "" {
		return
	}

	var urls []string
	for _, u := range strings.Split(rawURLs, "\n") {
		u = strings.TrimSpace(u)
		if u != "" {
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 {
		return
	}

	go func() {
		if err := b.notifier.Send(apiURL, urls, title, body, notifType); err != nil {
			log.Printf("Warning: failed to send %s notification: %v", eventKey, err)
		}
	}()
}

// toolheadNotifyInfo holds resolved spool data for a single toolhead,
// used by the message builders so they don't need DB access.
type toolheadNotifyInfo struct {
	ID              int
	SpoolID         int
	SpoolName       string
	Brand           string
	Material        string
	EstimateGrams   float64
	UsedGrams       float64
	RemainingWeight float64
	Mapped          bool
	Resolved        bool
}

func buildPrintStartedMessage(printerName, filename string, toolheads []toolheadNotifyInfo) (title, body, notifType string) {
	var b strings.Builder
	fmt.Fprintf(&b, "File: %s\n", filename)

	for _, th := range toolheads {
		if !th.Mapped {
			fmt.Fprintf(&b, "\nToolhead %d: No spool mapped\n  Estimated usage: %.1fg\n", th.ID, th.EstimateGrams)
		} else if !th.Resolved {
			fmt.Fprintf(&b, "\nToolhead %d: Spool %d (could not fetch details)\n  Estimated usage: %.1fg\n", th.ID, th.SpoolID, th.EstimateGrams)
		} else {
			fmt.Fprintf(&b, "\nToolhead %d: %s (%s %s)\n", th.ID, th.SpoolName, th.Brand, th.Material)
			fmt.Fprintf(&b, "  Estimated usage: %.1fg\n", th.EstimateGrams)
			fmt.Fprintf(&b, "  Remaining on spool: %.1fg\n", th.RemainingWeight)
			if th.EstimateGrams > th.RemainingWeight {
				fmt.Fprintf(&b, "  WARNING: Estimate exceeds remaining by %.1fg\n", th.EstimateGrams-th.RemainingWeight)
			}
		}
	}

	return fmt.Sprintf("Print Started on %s", printerName), b.String(), "info"
}

func buildPrintEndedMessage(printerName, filename, durationStr string, completed bool, usageScale float64, toolheads []toolheadNotifyInfo) (title, body, notifType string) {
	var t string
	if completed {
		t = fmt.Sprintf("Print Completed on %s", printerName)
		notifType = "success"
	} else {
		t = fmt.Sprintf("Print Failed on %s", printerName)
		notifType = "failure"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "File: %s\n", filename)
	if !completed {
		fmt.Fprintf(&b, "Progress: ~%.0f%%\n", usageScale*100)
	}
	fmt.Fprintf(&b, "Duration: %s\n", durationStr)

	for _, th := range toolheads {
		if !th.Mapped {
			fmt.Fprintf(&b, "\nToolhead %d: No spool mapped\n  Used: %.1fg\n", th.ID, th.UsedGrams)
		} else if !th.Resolved {
			fmt.Fprintf(&b, "\nToolhead %d: Spool %d\n  Used: %.1fg\n", th.ID, th.SpoolID, th.UsedGrams)
		} else {
			fmt.Fprintf(&b, "\nToolhead %d: %s (%s %s)\n", th.ID, th.SpoolName, th.Brand, th.Material)
			fmt.Fprintf(&b, "  Used: %.1fg\n", th.UsedGrams)
			fmt.Fprintf(&b, "  Remaining: %.1fg\n", th.RemainingWeight)
		}
	}

	return t, b.String(), notifType
}

func buildLowFilamentMessage(printerName, filename, spoolName string, spoolID int, remainingWeight, requiredWeight float64) (title, body, notifType string) {
	var b strings.Builder
	fmt.Fprintf(&b, "Spool \"%s\" (ID: %d) is running low.\n", spoolName, spoolID)
	fmt.Fprintf(&b, "Remaining: %.1fg\n", remainingWeight)
	fmt.Fprintf(&b, "Print needs: ~%.1fg\n", requiredWeight)
	fmt.Fprintf(&b, "Shortage: %.1fg\n", requiredWeight-remainingWeight)
	fmt.Fprintf(&b, "\nPrint at risk: %s on %s", filename, printerName)

	return fmt.Sprintf("Low Filament Warning: %s", printerName), b.String(), "warning"
}

func buildAutoPausedMessage(printerName, spoolName string, spoolID, toolheadID int, remainingWeight, requiredWeight float64) (title, body, notifType string) {
	var b strings.Builder
	fmt.Fprintf(&b, "The print has been paused due to low filament.\n\n")
	fmt.Fprintf(&b, "Spool \"%s\" (ID: %d) has %.1fg remaining.\n", spoolName, spoolID, remainingWeight)
	fmt.Fprintf(&b, "The print needs ~%.1fg to finish.\n\n", requiredWeight)
	fmt.Fprintf(&b, "To resume:\n")
	fmt.Fprintf(&b, "1. Swap the spool on Toolhead %d and remap it in FilaBridge\n", toolheadID)
	fmt.Fprintf(&b, "2. Acknowledge the warning on the FilaBridge dashboard (this resumes the print)\n")
	fmt.Fprintf(&b, "   OR resume directly from the printer")

	return fmt.Sprintf("Print Paused — Filament Insufficient: %s", printerName), b.String(), "warning"
}

func buildOfflineMessage(printerName, ipAddress string) (title, body, notifType string) {
	return fmt.Sprintf("Printer Offline: %s", printerName),
		fmt.Sprintf("Printer %s (%s) is no longer reachable.", printerName, ipAddress),
		"failure"
}

func buildOnlineMessage(printerName, ipAddress string) (title, body, notifType string) {
	return fmt.Sprintf("Printer Online: %s", printerName),
		fmt.Sprintf("Printer %s (%s) is back online.", printerName, ipAddress),
		"success"
}

// resolveToolheads fetches spool data for each toolhead in a job's usage map.
func (b *FilamentBridge) resolveToolheads(printerName string, usage map[int]float64, usageScale float64) []toolheadNotifyInfo {
	var toolheads []toolheadNotifyInfo
	for toolheadID, estimateGrams := range usage {
		th := toolheadNotifyInfo{
			ID:            toolheadID,
			EstimateGrams: estimateGrams,
			UsedGrams:     estimateGrams * usageScale,
		}
		spoolID, err := b.GetToolheadMapping(printerName, toolheadID)
		if err != nil || spoolID == 0 {
			toolheads = append(toolheads, th)
			continue
		}
		th.SpoolID = spoolID
		th.Mapped = true
		spool, err := b.spoolman.GetSpool(spoolID)
		if err != nil {
			toolheads = append(toolheads, th)
			continue
		}
		th.Resolved = true
		th.SpoolName = spool.Name
		th.Brand = spool.Brand
		th.Material = spool.Material
		th.RemainingWeight = spool.RemainingWeight
		toolheads = append(toolheads, th)
	}
	return toolheads
}

func (b *FilamentBridge) notifyPrintStarted(config PrinterConfig, aj *activeJob) {
	printerName := resolvePrinterName(config)
	toolheads := b.resolveToolheads(printerName, aj.Usage, 1.0)
	title, body, notifType := buildPrintStartedMessage(printerName, aj.Filename, toolheads)
	b.notify(ConfigKeyAppriseNotifyPrintStarted, title, body, notifType)
}

func (b *FilamentBridge) notifyPrintEnded(config PrinterConfig, active *activeJob, completed bool, usageScale float64) {
	printerName := resolvePrinterName(config)
	durationStr := formatDuration(time.Since(active.StartedAt))
	toolheads := b.resolveToolheads(printerName, active.Usage, usageScale)

	var eventKey string
	if completed {
		eventKey = ConfigKeyAppriseNotifyPrintDone
	} else {
		eventKey = ConfigKeyAppriseNotifyPrintFailed
	}

	title, body, notifType := buildPrintEndedMessage(printerName, active.Filename, durationStr, completed, usageScale, toolheads)
	b.notify(eventKey, title, body, notifType)
}

func (b *FilamentBridge) notifyLowFilament(config PrinterConfig, warning RunoutWarning, aj *activeJob) {
	title, body, notifType := buildLowFilamentMessage(warning.PrinterName, aj.Filename, warning.SpoolName, warning.SpoolID, warning.RemainingWeight, warning.RequiredWeight)
	b.notify(ConfigKeyAppriseNotifyLowFilament, title, body, notifType)
}

func (b *FilamentBridge) notifyAutoPaused(config PrinterConfig, warning RunoutWarning, aj *activeJob) {
	title, body, notifType := buildAutoPausedMessage(warning.PrinterName, warning.SpoolName, warning.SpoolID, warning.ToolheadID, warning.RemainingWeight, warning.RequiredWeight)
	b.notify(ConfigKeyAppriseNotifyAutoPaused, title, body, notifType)
}

func formatDuration(d time.Duration) string {
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
