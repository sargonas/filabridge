package main

// Bambu Lab printer support (local network only, no Bambu Cloud). Gated behind
// developer mode (FILABRIDGE_DEVELOPER_MODE) until complete.
//
// Transport: MQTT over TLS (ssl://<ip>:8883, user "bblp", pass = LAN access
// code) carries live state, progress, and the sliced filename. FTPS (port 990,
// implicit TLS) serves the sliced .3mf, whose Metadata/slice_info.config lists
// per-filament grams. Those grams feed the same processFilamentUsage() seam
// PrusaLink uses; an AMS slot maps to a "toolhead".
//
// This file currently implements the MQTT state half: a persistent client per
// printer that subscribes to the report topic, merges Bambu's partial reports
// into a cached state, and exposes it to the monitor loop. The FTPS fetch +
// slice_info parse and the active-job/usage wiring land next.

import (
	"archive/zip"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/jlaffaye/ftp"
)

const (
	bambuMQTTUser       = "bblp" // fixed LAN-mode MQTT username
	bambuMQTTPort       = 8883   // MQTT over TLS
	bambuFTPSPort       = 990    // implicit-TLS FTPS for the sliced .3mf
	bambuConnectTimeout = 10 * time.Second
	bambuKeepAlive      = 30 * time.Second
	bambuFTPSTimeout    = 20 * time.Second
)

// sliceInfoPath is the entry inside a Bambu .3mf (a zip) holding per-filament
// usage. bambuCacheDir is where cloud/slicer-sent prints land on the SD card.
const (
	sliceInfoPath = "Metadata/slice_info.config"
	bambuCacheDir = "cache"
)

// Bambu gcode_state values. The printer reports a small state machine; these are
// the transitions FilaBridge keys off of (running vs. terminal).
const (
	bambuStateIdle    = "IDLE"
	bambuStatePrepare = "PREPARE"
	bambuStateRunning = "RUNNING"
	bambuStatePause   = "PAUSE"
	bambuStateFinish  = "FINISH"
	bambuStateFailed  = "FAILED"
)

// bambuReport is the subset of Bambu's MQTT "report" payload FilaBridge needs.
// The printer emits partial reports (only changed fields per message), so each
// message is unmarshaled into the SAME cached struct: encoding/json leaves
// struct fields absent from the JSON untouched, which merges partials for free.
type bambuReport struct {
	Print bambuPrint `json:"print"`
}

type bambuPrint struct {
	GcodeState      string `json:"gcode_state"`       // IDLE/PREPARE/RUNNING/PAUSE/FINISH/FAILED
	GcodeFile       string `json:"gcode_file"`        // sliced file on the printer (path for FTPS fetch)
	SubtaskName     string `json:"subtask_name"`      // human-facing job name
	McPercent       int    `json:"mc_percent"`        // print progress, 0..100
	McRemainingTime int    `json:"mc_remaining_time"` // minutes remaining
	LayerNum        int    `json:"layer_num"`
	TotalLayerNum   int    `json:"total_layer_num"`
}

// bambuClient maintains one persistent MQTT connection to a Bambu printer and
// caches the latest merged report. paho handles reconnection; onConnect
// re-subscribes and re-requests a full state push, so the cache self-heals after
// a drop.
type bambuClient struct {
	ip         string
	serial     string
	accessCode string

	client mqtt.Client

	mu          sync.RWMutex
	report      bambuReport
	haveReport  bool      // false until the first report arrives
	lastMessage time.Time // last time any report was merged
}

func newBambuClient(ip, serial, accessCode string) *bambuClient {
	bc := &bambuClient{ip: ip, serial: serial, accessCode: accessCode}

	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("ssl://%s:%d", ip, bambuMQTTPort))
	opts.SetClientID(fmt.Sprintf("filabridge-%s", serial))
	opts.SetUsername(bambuMQTTUser)
	opts.SetPassword(accessCode)
	// Bambu printers present a self-signed certificate on the LAN. There is no
	// CA to validate against for a local device, so skip verification (the
	// connection is still encrypted, and it never leaves the local network).
	opts.SetTLSConfig(&tls.Config{InsecureSkipVerify: true}) // #nosec G402 - self-signed LAN device
	opts.SetConnectTimeout(bambuConnectTimeout)
	opts.SetKeepAlive(bambuKeepAlive)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(10 * time.Second)
	opts.SetOnConnectHandler(bc.onConnect)
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		log.Printf("Bambu %s: MQTT connection lost: %v", serial, err)
	})

	bc.client = mqtt.NewClient(opts)
	return bc
}

// reportTopic and requestTopic are the printer's per-serial MQTT topics.
func (bc *bambuClient) reportTopic() string  { return "device/" + bc.serial + "/report" }
func (bc *bambuClient) requestTopic() string { return "device/" + bc.serial + "/request" }

// connect opens the MQTT connection (non-blocking thereafter thanks to
// auto-reconnect). It returns an error only if the initial connect attempt
// fails outright.
func (bc *bambuClient) connect() error {
	token := bc.client.Connect()
	if !token.WaitTimeout(bambuConnectTimeout) {
		return fmt.Errorf("timed out connecting to Bambu MQTT at %s", bc.ip)
	}
	return token.Error()
}

// onConnect runs on every (re)connection: subscribe to the report topic and ask
// the printer to push its full current state, since ongoing reports are partial.
func (bc *bambuClient) onConnect(_ mqtt.Client) {
	if token := bc.client.Subscribe(bc.reportTopic(), 0, bc.onMessage); token.WaitTimeout(bambuConnectTimeout) && token.Error() != nil {
		log.Printf("Bambu %s: failed to subscribe to report topic: %v", bc.serial, token.Error())
		return
	}
	bc.requestPushAll()
	log.Printf("Bambu %s: MQTT connected (%s), subscribed and requested full state", bc.serial, bc.ip)
}

// requestPushAll asks the printer to emit a complete report (not just deltas).
func (bc *bambuClient) requestPushAll() {
	payload := `{"pushing":{"sequence_id":"0","command":"pushall"}}`
	bc.client.Publish(bc.requestTopic(), 0, false, payload)
}

// bambuDebugRaw, when true, logs every raw MQTT report payload. Set by the
// developer probe (main -bambu-probe) to inspect real printer messages.
var bambuDebugRaw bool

// onMessage merges an incoming (possibly partial) report into the cached state.
func (bc *bambuClient) onMessage(_ mqtt.Client, msg mqtt.Message) {
	if bambuDebugRaw {
		log.Printf("Bambu %s RAW report: %s", bc.serial, string(msg.Payload()))
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	// Decode into the existing struct so absent fields keep their prior value.
	if err := json.Unmarshal(msg.Payload(), &bc.report); err != nil {
		log.Printf("Bambu %s: failed to parse report: %v", bc.serial, err)
		return
	}
	bc.haveReport = true
	bc.lastMessage = time.Now()
}

// snapshot returns a copy of the cached report and whether any report has been
// received yet.
func (bc *bambuClient) snapshot() (bambuReport, bool) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.report, bc.haveReport
}

func (bc *bambuClient) isConnected() bool {
	return bc.client.IsConnected()
}

func (bc *bambuClient) disconnect() {
	bc.client.Disconnect(250)
}

// bambuJobName returns a human-facing name for the current Bambu job, preferring
// the subtask name and falling back to the sliced filename.
func bambuJobName(r bambuReport) string {
	if r.Print.SubtaskName != "" {
		return r.Print.SubtaskName
	}
	if r.Print.GcodeFile != "" {
		return r.Print.GcodeFile
	}
	return "No active job"
}

// ensureBambuClient returns the persistent MQTT client for a printer, creating
// and connecting it on first use. If the printer's address, serial, or access
// code changed, the old client is torn down and replaced.
func (b *FilamentBridge) ensureBambuClient(printerID string, config PrinterConfig) *bambuClient {
	b.bambuMutex.Lock()
	defer b.bambuMutex.Unlock()

	if existing, ok := b.bambuClients[printerID]; ok {
		if existing.ip == config.IPAddress && existing.serial == config.Serial && existing.accessCode == config.APIKey {
			return existing
		}
		existing.disconnect() // config changed; rebuild
		delete(b.bambuClients, printerID)
	}

	bc := newBambuClient(config.IPAddress, config.Serial, config.APIKey)
	b.bambuClients[printerID] = bc
	if err := bc.connect(); err != nil {
		// Auto-reconnect keeps trying in the background; log and return the
		// client so the next poll cycle sees it once it comes up.
		log.Printf("Bambu %s: initial MQTT connect failed (will keep retrying): %v", config.Name, err)
	}
	return bc
}

// bambuStateIsPrinting reports whether the printer is actively working a job.
func bambuStateIsPrinting(state string) bool {
	switch state {
	case bambuStateRunning, bambuStatePause, bambuStatePrepare:
		return true
	}
	return false
}

// bambuStateIsTerminal reports whether a job has ended (cleanly or otherwise).
func bambuStateIsTerminal(state string) bool {
	switch state {
	case bambuStateFinish, bambuStateFailed, bambuStateIdle:
		return true
	}
	return false
}

// bambuToToolheadUsage converts slice_info's 1-based filament ids to
// FilaBridge's 0-based toolhead numbering (external spool / AMS slot 1 -> 0).
func bambuToToolheadUsage(byFilament map[int]float64) map[int]float64 {
	out := make(map[int]float64, len(byFilament))
	for id, grams := range byFilament {
		toolhead := id - 1
		if toolhead < 0 {
			toolhead = 0
		}
		out[toolhead] = grams
	}
	return out
}

// bambuJobID synthesizes a stable, non-zero job id for a Bambu print. Local
// prints report task_id "0", so there is no natural id to dedupe on; filename +
// print-start time uniquely identifies a print and stays stable for the life of
// the tracked job (started_at is persisted and reused across poll cycles).
func bambuJobID(filename string, startedAt time.Time) int {
	h := fnv.New64a()
	_, _ = io.WriteString(h, filename)
	_, _ = io.WriteString(h, "|")
	_, _ = io.WriteString(h, strconv.FormatInt(startedAt.Unix(), 10))
	id := int(h.Sum64() & 0x7fffffffffffffff)
	if id == 0 {
		id = 1 // never collide with the "no stable id" sentinel (0)
	}
	return id
}

// bambuUsageFromFile downloads the sliced .3mf over FTPS and returns per-toolhead
// filament grams from slice_info.config.
func bambuUsageFromFile(ip, accessCode, gcodeFile string) (map[int]float64, error) {
	var lastErr error
	for _, remote := range bambuSlicedFileCandidates(gcodeFile) {
		data, err := fetchBambuFile(ip, accessCode, remote)
		if err != nil {
			lastErr = err
			continue
		}
		usage, err := parseSliceInfoUsage(data, 0)
		if err != nil {
			lastErr = err
			continue
		}
		return bambuToToolheadUsage(usage), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("sliced file %q not found on printer", gcodeFile)
	}
	return nil, lastErr
}

// fetchBambuUsage downloads the sliced .3mf over FTPS and returns per-toolhead
// filament grams from slice_info.config.
func (b *FilamentBridge) fetchBambuUsage(config PrinterConfig, gcodeFile string) (map[int]float64, error) {
	return bambuUsageFromFile(config.IPAddress, config.APIKey, gcodeFile)
}

// monitorBambu monitors a single Bambu printer for one poll cycle. It mirrors
// monitorPrusaLink over the MQTT-cached state: while printing it persists an
// in-flight job and captures the slicer's filament estimate (from the sliced
// .3mf over FTPS); on a terminal state it records usage against Spoolman,
// scaling a cancelled/failed print by the progress seen. State is read from the
// persistent MQTT client's cache.
func (b *FilamentBridge) monitorBambu(printerID string, config PrinterConfig) error {
	if config.Serial == "" {
		b.noteConnectivity(printerID, config.IPAddress, config.Name, fmt.Errorf("no serial configured"))
		return nil
	}
	client := b.ensureBambuClient(printerID, config)
	if !client.isConnected() {
		b.noteConnectivity(printerID, config.IPAddress, config.Name, fmt.Errorf("MQTT not connected"))
		return nil
	}
	b.noteConnectivity(printerID, config.IPAddress, config.Name, nil)

	report, ok := client.snapshot()
	if !ok {
		return nil // connected, awaiting the first report
	}
	p := report.Print
	state := p.GcodeState
	jobName := bambuJobName(report)
	currentFile := p.GcodeFile

	active, err := b.getActiveJob(printerID)
	if err != nil {
		log.Printf("Warning: failed to read active job for %s: %v", printerID, err)
	}

	b.noteStateChange(printerID, config.Name, state, jobName)

	switch {
	case bambuStateIsPrinting(state) && currentFile != "":
		aj := &activeJob{PrinterID: printerID, StartedAt: time.Now()}
		// Continue the tracked job when paused or when the same file is loaded.
		if active != nil && (state == bambuStatePause || active.Filename == currentFile) {
			aj.JobID = active.JobID
			aj.StartedAt = active.StartedAt
			aj.LastProgress = active.LastProgress
			aj.Usage = active.Usage
		}
		aj.Filename = currentFile
		if aj.JobID == 0 {
			aj.JobID = bambuJobID(currentFile, aj.StartedAt)
		}
		// Track monotonic max progress so a stale low reading can't shrink it.
		if state == bambuStateRunning {
			if prog := float64(p.McPercent) / 100.0; prog > aj.LastProgress {
				aj.LastProgress = prog
			}
		}
		// Capture the slicer estimate once, from the sliced .3mf over FTPS.
		// Bounded retries so a missing/locked file doesn't hammer the printer.
		if len(aj.Usage) == 0 && b.shouldScanForEstimate(printerID, currentFile) {
			if usage, err := b.fetchBambuUsage(config, currentFile); err != nil {
				log.Printf("Warning: could not fetch Bambu filament estimate for %s (will retry): %v", config.Name, err)
			} else if len(usage) > 0 {
				aj.Usage = usage
				log.Printf("Captured filament estimate for %s from sliced file: %v", config.Name, usage)
			}
			b.finishScanForEstimate(printerID)
		}
		if err := b.upsertActiveJob(aj); err != nil {
			log.Printf("Warning: failed to persist active job for %s: %v", printerID, err)
		}

	case bambuStateIsTerminal(state) && active != nil && active.Filename != "":
		completed := state == bambuStateFinish ||
			(state == bambuStateIdle && active.LastProgress >= PrintCompletionProgressThreshold)
		usageScale := 1.0
		if !completed {
			usageScale = active.LastProgress
		}

		// Idempotency: never record the same job twice (survives restart/re-detect).
		if recorded, err := b.isJobRecorded(printerID, active.JobID); err != nil {
			log.Printf("Warning: failed to check recorded-jobs ledger for %s job %d: %v", printerID, active.JobID, err)
		} else if recorded {
			b.clearActiveJob(printerID)
			return nil
		}

		// Guard against overlapping monitor cycles recording the same usage.
		b.mutex.Lock()
		if b.processingPrints[printerID] {
			b.mutex.Unlock()
			return nil
		}
		b.processingPrints[printerID] = true
		b.mutex.Unlock()
		defer func() {
			b.mutex.Lock()
			b.processingPrints[printerID] = false
			b.mutex.Unlock()
		}()

		if completed {
			log.Printf("Bambu print finished for %s: %s (state: %s, file: %s)", config.Name, jobName, state, active.Filename)
		} else {
			log.Printf("Bambu print cancelled/failed for %s: %s (state: %s, ~%.0f%% printed, file: %s)", config.Name, jobName, state, usageScale*100, active.Filename)
		}

		if err := b.handleBambuPrintEnded(config, active, usageScale, completed); err != nil {
			log.Printf("Error handling Bambu print end for %s: %v", printerID, err)
			b.clearActiveJob(printerID)
			return nil
		}
		if err := b.markJobRecorded(printerID, active.JobID, active.Filename, usageScale); err != nil {
			log.Printf("Warning: failed to mark job %d recorded for %s: %v", active.JobID, printerID, err)
		}
		b.clearActiveJob(printerID)

	case bambuStateIsTerminal(state) && active != nil && active.Filename == "":
		// Stale tracking row with no filename - drop it.
		b.clearActiveJob(printerID)
	}
	return nil
}

// handleBambuPrintEnded records filament usage for a Bambu print that has ended.
// It prefers the estimate captured while printing and falls back to one more
// FTPS fetch, then scales the estimate by usageScale (1.0 for a completed print,
// last-seen progress for a cancelled/failed one) before deducting from Spoolman.
func (b *FilamentBridge) handleBambuPrintEnded(config PrinterConfig, active *activeJob, usageScale float64, completed bool) error {
	printerName := resolvePrinterName(config)
	filename := active.Filename

	usage := active.Usage
	if len(usage) == 0 {
		// Estimate was never captured while printing; try once more now.
		if u, err := b.fetchBambuUsage(config, filename); err != nil {
			log.Printf("Warning: end-of-print estimate fetch failed for %s: %v", printerName, err)
		} else {
			usage = u
		}
	}
	if len(usage) == 0 {
		if usageScale == 0 {
			return nil // nothing printed and no estimate: nothing to record
		}
		msg := "no filament usage data found (slice_info)"
		b.addPrintError(printerName, filename, msg)
		return fmt.Errorf("%s", msg)
	}

	if usageScale < 0 {
		usageScale = 0
	}
	if usageScale < 1.0 {
		scaled := make(map[int]float64, len(usage))
		for toolhead, grams := range usage {
			scaled[toolhead] = grams * usageScale
		}
		usage = scaled
		log.Printf("Scaled Bambu filament usage to ~%.0f%% for partial print: %s", usageScale*100, filename)
	}

	printStarted := active.StartedAt
	if printStarted.IsZero() {
		printStarted = time.Now()
	}
	status := "completed"
	if !completed {
		status = "cancelled"
	}
	log.Printf("Recording Bambu filament usage for %s (%s): %+v", printerName, filename, usage)
	return b.processFilamentUsage(printerName, usage, filename, printStarted, status)
}

// bambuSliceInfo mirrors Metadata/slice_info.config inside a Bambu .3mf. Each
// plate lists its filaments with the slicer's grams estimate (used_g), which is
// the value FilaBridge deducts from Spoolman (MQTT never reports grams).
type bambuSliceInfo struct {
	XMLName xml.Name          `xml:"config"`
	Plates  []bambuSlicePlate `xml:"plate"`
}

type bambuSlicePlate struct {
	Metadata  []bambuSliceKV       `xml:"metadata"`
	Filaments []bambuSliceFilament `xml:"filament"`
}

type bambuSliceKV struct {
	Key   string `xml:"key,attr"`
	Value string `xml:"value,attr"`
}

type bambuSliceFilament struct {
	ID    string `xml:"id,attr"`    // 1-based filament/AMS-slot index
	Type  string `xml:"type,attr"`  // e.g. "PLA"
	Color string `xml:"color,attr"` // "#RRGGBB"
	UsedM string `xml:"used_m,attr"`
	UsedG string `xml:"used_g,attr"` // grams for this filament on this plate
}

// index returns the plate's 1-based index from its metadata, or 0 if absent.
func (p *bambuSlicePlate) index() int {
	for _, m := range p.Metadata {
		if m.Key == "index" {
			if idx, err := strconv.Atoi(strings.TrimSpace(m.Value)); err == nil {
				return idx
			}
		}
	}
	return 0
}

// fetchBambuFile downloads a file from a Bambu printer over implicit-TLS FTPS
// (port 990, user "bblp", pass = access code). The printer uses a self-signed
// certificate, so verification is skipped (LAN-only, still encrypted).
func fetchBambuFile(ip, accessCode, remotePath string) ([]byte, error) {
	conn, err := ftp.Dial(net.JoinHostPort(ip, strconv.Itoa(bambuFTPSPort)),
		ftp.DialWithTimeout(bambuFTPSTimeout),
		ftp.DialWithTLS(&tls.Config{InsecureSkipVerify: true}), // #nosec G402 - self-signed LAN device
	)
	if err != nil {
		return nil, fmt.Errorf("FTPS dial: %w", err)
	}
	defer func() { _ = conn.Quit() }()

	if err := conn.Login(bambuMQTTUser, accessCode); err != nil {
		return nil, fmt.Errorf("FTPS login: %w", err)
	}
	r, err := conn.Retr(remotePath)
	if err != nil {
		return nil, fmt.Errorf("FTPS retrieve %q: %w", remotePath, err)
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

// bambuSlicedFileCandidates lists the FTPS paths a sliced file may live at. A
// cloud/slicer-sent print lands in the SD card's cache dir; an SD print sits at
// the root.
func bambuSlicedFileCandidates(gcodeFile string) []string {
	name := strings.TrimPrefix(gcodeFile, "/")
	return []string{
		bambuCacheDir + "/" + name, // cache/foo.gcode.3mf
		name,                       // foo.gcode.3mf (root)
	}
}

// extractSliceInfoXML pulls Metadata/slice_info.config out of a .3mf (a zip).
func extractSliceInfoXML(threemf []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(threemf), int64(len(threemf)))
	if err != nil {
		return nil, fmt.Errorf("open 3mf zip: %w", err)
	}
	for _, f := range zr.File {
		if strings.EqualFold(f.Name, sliceInfoPath) {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer func() { _ = rc.Close() }()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("%s not found in 3mf", sliceInfoPath)
}

// parseSliceInfoUsage returns per-filament grams for the given 1-based plate
// index (0 selects the sole plate). Keys are the filament IDs as they appear in
// slice_info (1-based); the caller maps them to FilaBridge toolheads.
func parseSliceInfoUsage(threemf []byte, plateIndex int) (map[int]float64, error) {
	xmlData, err := extractSliceInfoXML(threemf)
	if err != nil {
		return nil, err
	}
	var info bambuSliceInfo
	if err := xml.Unmarshal(xmlData, &info); err != nil {
		return nil, fmt.Errorf("parse slice_info: %w", err)
	}
	if len(info.Plates) == 0 {
		return nil, fmt.Errorf("no plates in slice_info")
	}

	plate := &info.Plates[0]
	if plateIndex > 0 {
		for i := range info.Plates {
			if info.Plates[i].index() == plateIndex {
				plate = &info.Plates[i]
				break
			}
		}
	}

	usage := make(map[int]float64)
	for _, fil := range plate.Filaments {
		id, err := strconv.Atoi(strings.TrimSpace(fil.ID))
		if err != nil {
			continue
		}
		grams, err := strconv.ParseFloat(strings.TrimSpace(fil.UsedG), 64)
		if err != nil || grams <= 0 {
			continue // skip unused slots
		}
		usage[id] = grams
	}
	return usage, nil
}

// probeBambuFTPS is the FTPS half of the developer probe: it logs the SD-card
// listing, downloads the sliced file for the current job, and prints the
// per-filament grams parsed from slice_info.config.
func probeBambuFTPS(ip, accessCode, gcodeFile string) {
	log.Printf("Bambu probe: FTPS connecting to %s:%d ...", ip, bambuFTPSPort)
	conn, err := ftp.Dial(net.JoinHostPort(ip, strconv.Itoa(bambuFTPSPort)),
		ftp.DialWithTimeout(bambuFTPSTimeout),
		ftp.DialWithTLS(&tls.Config{InsecureSkipVerify: true}), // #nosec G402 - self-signed LAN device
	)
	if err != nil {
		log.Printf("Bambu probe: FTPS dial failed: %v", err)
		return
	}
	defer func() { _ = conn.Quit() }()
	if err := conn.Login(bambuMQTTUser, accessCode); err != nil {
		log.Printf("Bambu probe: FTPS login failed: %v", err)
		return
	}
	log.Printf("Bambu probe: FTPS logged in; listing root and cache:")
	for _, dir := range []string{"/", "/" + bambuCacheDir} {
		entries, err := conn.List(dir)
		if err != nil {
			log.Printf("  LIST %s: %v", dir, err)
			continue
		}
		for _, e := range entries {
			log.Printf("  %s -> %s (%d bytes)", dir, e.Name, e.Size)
		}
	}

	if gcodeFile == "" {
		log.Printf("Bambu probe: no gcode_file from MQTT; skipping download")
		return
	}
	for _, p := range bambuSlicedFileCandidates(gcodeFile) {
		data, err := fetchBambuFile(ip, accessCode, p)
		if err != nil {
			log.Printf("Bambu probe: fetch %q: %v", p, err)
			continue
		}
		log.Printf("Bambu probe: downloaded %q (%d bytes)", p, len(data))
		if xmlData, err := extractSliceInfoXML(data); err == nil {
			log.Printf("Bambu probe: raw slice_info.config:\n%s", string(xmlData))
		}
		usage, err := parseSliceInfoUsage(data, 0)
		if err != nil {
			log.Printf("Bambu probe: slice_info parse failed: %v", err)
			return
		}
		log.Printf("Bambu probe: per-filament grams (filament id -> g): %v", usage)
		return
	}
	log.Printf("Bambu probe: could not locate %q via FTPS", path.Base(gcodeFile))
}

// runBambuProbe is a developer tool (main -bambu-probe): it connects to a Bambu
// printer over MQTT using the real client, dumps every raw report for the given
// duration, then prints the final parsed state. It validates the LAN
// connection, credentials, and report schema against a real printer without
// needing a configured install.
func runBambuProbe(ip, serial, code string, dur time.Duration) {
	bambuDebugRaw = true
	log.Printf("Bambu probe: connecting to %s (serial %s)...", ip, serial)
	bc := newBambuClient(ip, serial, code)
	if err := bc.connect(); err != nil {
		log.Fatalf("Bambu probe: connect failed: %v", err)
	}
	defer bc.disconnect()
	log.Printf("Bambu probe: connected=%v; waiting for first report...", bc.isConnected())

	// Wait (up to the budget) for a report carrying the sliced filename.
	deadline := time.Now().Add(dur)
	var report bambuReport
	for time.Now().Before(deadline) {
		if r, ok := bc.snapshot(); ok && r.Print.GcodeFile != "" {
			report = r
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	if report.Print.GcodeFile == "" {
		if r, ok := bc.snapshot(); ok {
			log.Printf("Bambu probe: state=%q job=%q but no gcode_file (idle printer has no active sliced file)", r.Print.GcodeState, bambuJobName(r))
		} else {
			log.Printf("Bambu probe: no reports received. Check IP/serial/access code and that the printer is in LAN Mode.")
		}
		return
	}

	log.Printf("Bambu probe: state=%q job=%q progress=%d%% file=%q",
		report.Print.GcodeState, bambuJobName(report), report.Print.McPercent, report.Print.GcodeFile)

	// Validate the FTPS + slice_info half against the live file.
	probeBambuFTPS(ip, code, report.Print.GcodeFile)
}

// runBambuWatch is a developer tool (main -bambu-watch): it tails a printer's
// live state, logging every state change and progress, until it observes a
// terminal transition after a running print, then fetches and prints the final
// per-toolhead grams the state machine would record. Used to validate
// end-of-print detection against a real print.
func runBambuWatch(ip, serial, code string, dur time.Duration) {
	log.Printf("Bambu watch: connecting to %s (serial %s), watching up to %s...", ip, serial, dur)
	bc := newBambuClient(ip, serial, code)
	if err := bc.connect(); err != nil {
		log.Fatalf("Bambu watch: connect failed: %v", err)
	}
	defer bc.disconnect()

	deadline := time.Now().Add(dur)
	lastState, lastPct := "", -1
	sawRunning := false
	lastFile := "" // Bambu clears gcode_file at FINISH; remember it while printing.
	for time.Now().Before(deadline) {
		if r, ok := bc.snapshot(); ok {
			st := r.Print.GcodeState
			if r.Print.GcodeFile != "" {
				lastFile = r.Print.GcodeFile
			}
			if st != lastState {
				log.Printf("Bambu watch: STATE %q -> %q (job=%q, %d%%, file=%q)", lastState, st, bambuJobName(r), r.Print.McPercent, r.Print.GcodeFile)
				lastState = st
			}
			if r.Print.McPercent != lastPct {
				lastPct = r.Print.McPercent
				log.Printf("Bambu watch: progress %d%% (state %q)", lastPct, st)
			}
			if st == bambuStateRunning {
				sawRunning = true
			}
			if sawRunning && bambuStateIsTerminal(st) {
				completed := st == bambuStateFinish
				log.Printf("Bambu watch: TERMINAL %q at %d%% (completed=%v). Fetching final estimate for %q...", st, r.Print.McPercent, completed, lastFile)
				usage, err := bambuUsageFromFile(ip, code, lastFile)
				if err != nil {
					log.Printf("Bambu watch: estimate fetch failed: %v", err)
				} else {
					log.Printf("Bambu watch: per-toolhead grams that WOULD be recorded: %v (status=%s)", usage, map[bool]string{true: "completed", false: "cancelled"}[completed])
				}
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	log.Printf("Bambu watch: deadline reached without a terminal transition (last state %q, %d%%)", lastState, lastPct)
}
