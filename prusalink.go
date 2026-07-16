package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// PrusaLinkClient handles communication with PrusaLink API
type PrusaLinkClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// PrusaLinkStatus represents the status response from PrusaLink
type PrusaLinkStatus struct {
	Printer struct {
		State       string `json:"state"`
		Temperature struct {
			Bed struct {
				Actual float64 `json:"actual"`
				Target float64 `json:"target"`
			} `json:"bed"`
			Tool0 struct {
				Actual float64 `json:"actual"`
				Target float64 `json:"target"`
			} `json:"tool0"`
			Tool1 struct {
				Actual float64 `json:"actual"`
				Target float64 `json:"target"`
			} `json:"tool1,omitempty"`
			Tool2 struct {
				Actual float64 `json:"actual"`
				Target float64 `json:"target"`
			} `json:"tool2,omitempty"`
			Tool3 struct {
				Actual float64 `json:"actual"`
				Target float64 `json:"target"`
			} `json:"tool3,omitempty"`
			Tool4 struct {
				Actual float64 `json:"actual"`
				Target float64 `json:"target"`
			} `json:"tool4,omitempty"`
		} `json:"temperature"`
		Telemetry struct {
			PrintTime     int     `json:"print_time"`
			PrintTimeLeft int     `json:"print_time_left"`
			Progress      float64 `json:"progress"`
		} `json:"telemetry"`
	} `json:"printer"`
}

// PrusaLinkJob represents the job response from PrusaLink
type PrusaLinkJob struct {
	ID            int     `json:"id"`
	State         string  `json:"state"`
	Progress      float64 `json:"progress"`
	TimeRemaining int     `json:"time_remaining"`
	TimePrinting  int     `json:"time_printing"`
	File          struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Path        string `json:"path"`
		Size        int    `json:"size"`
		Refs        struct {
			Download string `json:"download"`
		} `json:"refs"`
		// Meta holds the slicer metadata PrusaLink indexes for the print file,
		// including "filament used [g]" per toolhead. Available while the job is
		// loaded/printing; typically absent once the printer returns to idle.
		Meta map[string]interface{} `json:"meta,omitempty"`
	} `json:"file"`
	// Filament usage data (if available)
	Filament []struct {
		ToolheadID int     `json:"toolhead_id"`
		Length     float64 `json:"length"`
		Weight     float64 `json:"weight"`
	} `json:"filament,omitempty"`
}

// NewPrusaLinkClient creates a new PrusaLink client
func NewPrusaLinkClient(ipAddress, apiKey string, timeout, fileDownloadTimeout int) *PrusaLinkClient {
	// Create a custom dialer with timeout for DNS resolution
	// This ensures hostnames (especially .local domains) have adequate time to resolve
	dialer := &net.Dialer{
		Timeout:   5 * time.Second, // DNS resolution timeout
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		MaxIdleConns:          10,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second, // Timeout for receiving response headers
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &PrusaLinkClient{
		baseURL: fmt.Sprintf("http://%s", ipAddress),
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout:   time.Duration(timeout) * time.Second,
			Transport: transport,
		},
	}
}

// addAPIKey adds API key authentication to the request
func (c *PrusaLinkClient) addAPIKey(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("X-Api-Key", c.apiKey)
	}
}

// GetStatus retrieves the current status of the printer
func (c *PrusaLinkClient) GetStatus() (*PrusaLinkStatus, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/api/v1/status", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create status request: %w", err)
	}

	c.addAPIKey(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get status from PrusaLink: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("PrusaLink API error: %d - %s", resp.StatusCode, string(body))
	}

	var status PrusaLinkStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("failed to decode status response: %w", err)
	}

	return &status, nil
}

// GetJobInfo retrieves the current job information
func (c *PrusaLinkClient) GetJobInfo() (*PrusaLinkJob, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/api/v1/job", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create job request: %w", err)
	}

	c.addAPIKey(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get job info from PrusaLink: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("PrusaLink API error: %d - %s", resp.StatusCode, string(body))
	}

	// Handle 204 No Content (no active job)
	if resp.StatusCode == http.StatusNoContent {
		return &PrusaLinkJob{}, nil
	}

	var job PrusaLinkJob
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return nil, fmt.Errorf("failed to decode job response: %w", err)
	}

	return &job, nil
}

// jobCommand issues a PUT to a v1 job sub-endpoint (pause/resume). PrusaLink
// responds 204 on success.
func (c *PrusaLinkClient) jobCommand(jobID int, command string) error {
	req, err := http.NewRequest("PUT", fmt.Sprintf("%s/api/v1/job/%d/%s", c.baseURL, jobID, command), nil)
	if err != nil {
		return fmt.Errorf("failed to create %s request: %w", command, err)
	}
	c.addAPIKey(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to %s job %d: %w", command, jobID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("PrusaLink %s error: %d - %s", command, resp.StatusCode, string(body))
	}
	return nil
}

// PauseJob pauses the given print job
func (c *PrusaLinkClient) PauseJob(jobID int) error {
	return c.jobCommand(jobID, "pause")
}

// ResumeJob resumes the given paused print job
func (c *PrusaLinkClient) ResumeJob(jobID int) error {
	return c.jobCommand(jobID, "resume")
}

// GetGcodeFile downloads the G-code file for a completed print job
func (c *PrusaLinkClient) GetGcodeFile(filename string) ([]byte, error) {
	// Use the correct PrusaLink API format: /{filename}
	// The filename should already include the full path (e.g., "usb/SHAPE-~1.BGC")
	req, err := http.NewRequest("GET", c.baseURL+"/"+filename, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create G-code request: %w", err)
	}

	c.addAPIKey(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get G-code file from PrusaLink: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("PrusaLink API error: %d - %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read G-code file: %w", err)
	}

	return body, nil
}

// GetGcodeFileWithRetry downloads the G-code file with retry logic and exponential backoff
func (c *PrusaLinkClient) GetGcodeFileWithRetry(filename string, fileDownloadTimeout int) ([]byte, error) {
	const maxRetries = 3
	backoffDelays := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}

	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		log.Printf("Downloading G-code file attempt %d/%d: %s", attempt+1, maxRetries, filename)

		// Create a new client with extended timeout for file downloads
		// Use the same DNS timeout configuration for consistency
		fileDialer := &net.Dialer{
			Timeout:   5 * time.Second, // DNS resolution timeout
			KeepAlive: 30 * time.Second,
		}

		fileClient := &http.Client{
			Timeout: time.Duration(fileDownloadTimeout) * time.Second,
			Transport: &http.Transport{
				DialContext:           fileDialer.DialContext,
				MaxIdleConns:          10,
				MaxIdleConnsPerHost:   2,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		}

		// Use the correct PrusaLink API format: /{filename}
		req, err := http.NewRequest("GET", c.baseURL+"/"+filename, nil)
		if err != nil {
			lastErr = fmt.Errorf("failed to create G-code request: %w", err)
			log.Printf("Attempt %d failed: %v", attempt+1, lastErr)
			if attempt < maxRetries-1 {
				time.Sleep(backoffDelays[attempt])
			}
			continue
		}

		c.addAPIKey(req)

		resp, err := fileClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("failed to get G-code file from PrusaLink: %w", err)
			log.Printf("Attempt %d failed: %v", attempt+1, lastErr)
			if attempt < maxRetries-1 {
				time.Sleep(backoffDelays[attempt])
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("PrusaLink API error: %d - %s", resp.StatusCode, string(body))
			log.Printf("Attempt %d failed: %v", attempt+1, lastErr)
			if attempt < maxRetries-1 {
				time.Sleep(backoffDelays[attempt])
			}
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("failed to read G-code file: %w", err)
			log.Printf("Attempt %d failed: %v", attempt+1, lastErr)
			if attempt < maxRetries-1 {
				time.Sleep(backoffDelays[attempt])
			}
			continue
		}

		// Success!
		log.Printf("Successfully downloaded G-code file on attempt %d: %s (%d bytes)",
			attempt+1, filename, len(body))
		return body, nil
	}

	return nil, fmt.Errorf("failed to download G-code file after %d attempts: %w", maxRetries, lastErr)
}

// filamentUsedRegex matches the slicer's "filament used [g]" line in both
// formats:
//   - .bgcode metadata block: "filament used [g]=1.23,4.56"
//   - .gcode comment:         "; filament used [g] = 1.23, 4.56"
var filamentUsedRegex = regexp.MustCompile(`;?\s*filament used \[g\]\s*=\s*([0-9.,\s]+)`)

// parseFilamentWeights parses the comma-separated per-toolhead weights from a
// filamentUsedRegex capture group.
func parseFilamentWeights(weightsStr string) map[int]float64 {
	filamentUsage := make(map[int]float64)
	for i, weightStr := range strings.Split(weightsStr, ",") {
		weightStr = strings.TrimSpace(weightStr)
		if weight, err := strconv.ParseFloat(weightStr, 64); err == nil && weight > 0 {
			filamentUsage[i] = weight
		}
	}
	return filamentUsage
}

// ParseGcodeFilamentUsage extracts filament usage from .gcode or .bgcode content
func (c *PrusaLinkClient) ParseGcodeFilamentUsage(gcodeContent []byte) (map[int]float64, error) {
	if match := filamentUsedRegex.FindSubmatch(gcodeContent); len(match) >= 2 {
		if usage := parseFilamentWeights(string(match[1])); len(usage) > 0 {
			return usage, nil
		}
	}
	// No filament usage data found; both .gcode and .bgcode files should
	// contain this metadata when generated by slicers
	return map[int]float64{}, nil
}

// GcodeScanLimit caps how much of a print file ScanGcodeFilamentUsage will
// stream before giving up. Binary G-code stores its metadata block within the
// first few KB, and ASCII files under this size are covered end to end.
const GcodeScanLimit = 16 << 20 // 16 MB

// ScanGcodeFilamentUsage streams the print file and returns the slicer's
// per-toolhead filament weights, closing the connection as soon as the values
// are found. PrusaLink serves files extremely slowly (tens of KB/s) and does
// not support HTTP range requests, so full downloads of large files cannot
// finish inside any reasonable timeout - but .bgcode keeps "filament used [g]"
// in its metadata block near the start of the file, which typically makes this
// a two-second read regardless of file size. ASCII .gcode stores the values at
// the end of the file instead, so files larger than GcodeScanLimit return an
// empty map and the caller should fall back to a full download.
func (c *PrusaLinkClient) ScanGcodeFilamentUsage(filename string, timeout int) (map[int]float64, error) {
	client := &http.Client{
		Timeout:   time.Duration(timeout) * time.Second,
		Transport: c.httpClient.Transport,
	}

	req, err := http.NewRequest("GET", c.baseURL+"/"+filename, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create G-code scan request: %w", err)
	}
	c.addAPIKey(req)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to start G-code scan: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("PrusaLink API error: %d - %s", resp.StatusCode, string(body))
	}

	// Stream in chunks, scanning a sliding window of the last overlap bytes
	// plus the new chunk (never the whole accumulated stream, which would be
	// quadratic). The overlap ensures a metadata line split across a chunk
	// boundary is still seen whole, and a match near the window's end is only
	// trusted once more data has arrived past it (or at EOF), so a value is
	// never parsed truncated.
	const chunkSize = 64 << 10
	const overlap = 512 // longer than any "filament used [g]=..." line
	var totalRead int
	window := make([]byte, 0, chunkSize+overlap)
	chunk := make([]byte, chunkSize)
	for {
		n, readErr := resp.Body.Read(chunk)
		eof := readErr == io.EOF
		if n > 0 || eof {
			window = append(window, chunk[:n]...)
			totalRead += n
			if loc := filamentUsedRegex.FindSubmatchIndex(window); loc != nil {
				if eof || loc[1] < len(window)-64 {
					if usage := parseFilamentWeights(string(window[loc[2]:loc[3]])); len(usage) > 0 {
						return usage, nil
					}
				}
			}
			// Keep only the tail as the next window's prefix
			if len(window) > overlap {
				copy(window, window[len(window)-overlap:])
				window = window[:overlap]
			}
		}
		if eof {
			return map[int]float64{}, nil // whole file scanned, nothing found
		}
		if readErr != nil {
			return nil, fmt.Errorf("failed while scanning G-code file: %w", readErr)
		}
		if totalRead >= GcodeScanLimit {
			return map[int]float64{}, nil // give up; caller may fall back to a full download
		}
	}
}

// filamentUsageFromMeta extracts per-toolhead filament usage (grams) from a
// PrusaLink job's file.meta. It reads the same "filament used [g]" field that
// ParseGcodeFilamentUsage pulls from the G-code file, so callers get identical
// map[toolheadID]grams output without downloading the file. Returns an empty
// map when the metadata is absent or unparseable.
func filamentUsageFromMeta(meta map[string]interface{}) map[int]float64 {
	usage := make(map[int]float64)
	if meta == nil {
		return usage
	}

	raw, ok := meta["filament used [g]"]
	if !ok {
		return usage
	}

	addValue := func(i int, s string) {
		s = strings.TrimSpace(s)
		if v, err := strconv.ParseFloat(s, 64); err == nil && v > 0 {
			usage[i] = v
		}
	}

	switch val := raw.(type) {
	case string:
		// "1.23,4.56" (per toolhead) or a single "1.23"
		for i, part := range strings.Split(val, ",") {
			addValue(i, part)
		}
	case float64:
		if val > 0 {
			usage[0] = val
		}
	case []interface{}:
		for i, item := range val {
			switch n := item.(type) {
			case float64:
				if n > 0 {
					usage[i] = n
				}
			case string:
				addValue(i, n)
			}
		}
	}

	return usage
}

// TestConnection tests the connection to PrusaLink
func (c *PrusaLinkClient) TestConnection() error {
	_, err := c.GetStatus()
	return err
}
