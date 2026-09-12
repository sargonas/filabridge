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
// A persistent client per printer subscribes to the report topic and merges
// Bambu's partial reports into a cached state for the monitor loop, which
// tracks the in-flight job, records usage at print end, and drives the shared
// low-filament warning path over MQTT print commands.
//
// Still incomplete: toolheads are a flat index, so the AMS mapping the printer
// reports can only be placed where it fits the configured count (the external
// spool holder is the last toolhead), and anything else falls back to the
// slicer's filament order.

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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	bambuCommandTimeout = 5 * time.Second // publish ack for pause/resume
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

	// Parsed out of band by mergeLenient, never by the main decode. Bambu's
	// field types vary by model and firmware, and a type mismatch in the main
	// decode would reject the whole report, taking the printer's state with it.
	PlateIdx int            `json:"-"` // plate being printed, 0 when not reported
	Mapping  []int          `json:"-"` // AMS tray (or bambuExternalSpool) per slicer filament
	Sources  map[int]string `json:"-"` // mapping value -> material loaded there
}

// bambuExternalSpool is the value a job's mapping uses for a filament fed from
// the external spool holder rather than an AMS tray.
const bambuExternalSpool = 65280

// mergeLenient picks plate_idx and mapping out of a report. Like every other
// field they are left alone when a report does not carry them. A value of an
// unexpected type is treated as not reported rather than failing the report.
// Both are assigned fresh values, never written in place, so a snapshot taken
// earlier keeps a stable copy.
func (p *bambuPrint) mergeLenient(payload []byte) {
	var raw struct {
		Print struct {
			PlateIdx json.RawMessage `json:"plate_idx"`
			Mapping  json.RawMessage `json:"mapping"`
		} `json:"print"`
	}
	if json.Unmarshal(payload, &raw) != nil {
		return
	}
	if len(raw.Print.PlateIdx) > 0 {
		p.PlateIdx = lenientInt(raw.Print.PlateIdx)
	}
	if len(raw.Print.Mapping) > 0 {
		var m []int
		if json.Unmarshal(raw.Print.Mapping, &m) != nil {
			m = nil
		}
		p.Mapping = m
	}
	// Only a full report carries the filament blocks. A delta leaves the last
	// answer standing, like every other field here.
	if sources := parseBambuSources(payload); sources != nil {
		p.Sources = sources
	}
}

// lenientInt reads a JSON number or a numeric string, and 0 for anything else.
func lenientInt(raw json.RawMessage) int {
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return n
		}
	}
	return 0
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
	seq    atomic.Uint64 // sequence_id for outgoing commands

	mu          sync.RWMutex
	report      bambuReport
	haveReport  bool      // false until the first report arrives
	lastMessage time.Time // last time any report was merged
	// lastFull keeps the most recent complete state push verbatim. The printer
	// sends one on connect (in answer to pushall) and only small deltas after,
	// so this is the one payload that carries the whole schema - AMS trays
	// included - for diagnostics that need more than the parsed subset.
	lastFull []byte

	// layout is what the printer last said it is made of, and layoutSeq counts
	// the times that answer changed. The monitor loop reconciles the printer's
	// filament positions only when the count moves, rather than on every report:
	// an X2D sends a full state push every few seconds.
	layout    bambuLayout
	layoutSeq uint64
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
		// Drop the cached report: whatever the printer was last doing is no
		// longer something we know. onConnect re-requests a full state push, so
		// the cache refills as soon as the printer is back.
		bc.invalidateReport()
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

// sendPrintCommand publishes a "print" command (pause/resume/stop) on the
// printer's request topic. Bambu acknowledges nothing beyond the MQTT publish,
// so the caller confirms the effect by watching gcode_state in the next report.
func (bc *bambuClient) sendPrintCommand(command string) error {
	// isConnected, not paho's IsConnected: a publish into a session that is only
	// reconnecting would be reported as a successful pause that never happened.
	if !bc.isConnected() {
		return fmt.Errorf("MQTT not connected to %s", bc.ip)
	}
	payload := fmt.Sprintf(`{"print":{"sequence_id":"%d","command":"%s","param":""}}`,
		bc.seq.Add(1), command)
	token := bc.client.Publish(bc.requestTopic(), 0, false, payload)
	if !token.WaitTimeout(bambuCommandTimeout) {
		return fmt.Errorf("timed out publishing %q to %s", command, bc.ip)
	}
	return token.Error()
}

// PauseJob and ResumeJob satisfy jobController. Bambu's commands act on whatever
// the printer is currently running rather than on a numbered job, so the job id
// is accepted and ignored; it stays in the signature so a Bambu printer and a
// PrusaLink one drive the same low-filament path.
func (bc *bambuClient) PauseJob(int) error  { return bc.sendPrintCommand("pause") }
func (bc *bambuClient) ResumeJob(int) error { return bc.sendPrintCommand("resume") }

// IsPaused reports whether the printer is paused, from the cached MQTT state.
// Unlike the PrusaLink implementation this costs the printer nothing, but it
// needs at least one report to have arrived.
func (bc *bambuClient) IsPaused() (bool, error) {
	report, ok := bc.snapshot()
	if !ok {
		return false, fmt.Errorf("no state reported yet by %s", bc.ip)
	}
	return report.Print.GcodeState == bambuStatePause, nil
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
	bc.report.Print.mergeLenient(msg.Payload())
	bc.haveReport = true
	bc.lastMessage = time.Now()
	// A complete push carries the AMS block; the frequent deltas do not. Keep
	// the full one rather than letting a temperature update overwrite it.
	if bytes.Contains(msg.Payload(), []byte(`"ams"`)) {
		bc.lastFull = append([]byte(nil), msg.Payload()...)
	}
	// Only a complete push can say what the printer has. A delta says nothing
	// about the layout, and must leave the last answer standing rather than
	// reading as a printer that lost its AMS.
	if layout, complete := parseBambuLayout(msg.Payload()); complete {
		if bambuLayoutSignature(layout) != bambuLayoutSignature(bc.layout) {
			bc.layout = layout
			bc.layoutSeq++
		}
	}
}

// layoutSnapshot returns the printer's last known layout and the count of
// changes, so a caller can tell whether it has already acted on this one.
func (bc *bambuClient) layoutSnapshot() (bambuLayout, uint64) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.layout, bc.layoutSeq
}

// snapshot returns a copy of the cached report and whether any report has been
// received yet.
func (bc *bambuClient) snapshot() (bambuReport, bool) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.report, bc.haveReport
}

// fullReportJSON returns the last complete state push, or nil if none has been
// seen yet.
func (bc *bambuClient) fullReportJSON() []byte {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.lastFull
}

// invalidateReport forgets the cached state, so a reader gets "nothing known"
// rather than a report from before the printer went away.
func (bc *bambuClient) invalidateReport() {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	bc.haveReport = false
}

// isConnected reports whether the MQTT session is actually open.
//
// It deliberately asks IsConnectionOpen rather than IsConnected: with
// auto-reconnect and connect-retry both enabled (they are, above), paho's
// IsConnected also answers true while it is merely *trying* to reconnect. A
// printer that has been powered off would therefore read as connected forever,
// and its last cached report would keep being served as current state.
// IsConnectionOpen is true only for a live session.
func (bc *bambuClient) isConnected() bool {
	return bc.client.IsConnectionOpen()
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
//
// The new client is registered before it is dialed, and the dial happens outside
// bambuMutex: connect() blocks for up to bambuConnectTimeout, and holding the
// map lock across it would serialize every other printer's setup behind an
// unreachable one. A caller that arrives mid-dial gets the same client rather
// than building a second connection to the same printer.
func (b *FilamentBridge) ensureBambuClient(printerID string, config PrinterConfig) *bambuClient {
	b.bambuMutex.Lock()
	if existing, ok := b.bambuClients[printerID]; ok {
		if existing.ip == config.IPAddress && existing.serial == config.Serial && existing.accessCode == config.APIKey {
			b.bambuMutex.Unlock()
			return existing
		}
		existing.disconnect() // config changed; rebuild
		delete(b.bambuClients, printerID)
	}
	bc := newBambuClient(config.IPAddress, config.Serial, config.APIKey)
	b.bambuClients[printerID] = bc
	b.bambuMutex.Unlock()

	if err := bc.connect(); err != nil {
		// Auto-reconnect keeps trying in the background; log and return the
		// client so the next poll cycle sees it once it comes up.
		log.Printf("Bambu %s: initial MQTT connect failed (will keep retrying): %v", config.Name, err)
	}
	return bc
}

// existingBambuClient returns the MQTT client already established for a printer,
// or nil if none has been created. Unlike ensureBambuClient it never dials, so a
// status read can consult live MQTT state without blocking on a TLS handshake.
func (b *FilamentBridge) existingBambuClient(printerID string) *bambuClient {
	b.bambuMutex.Lock()
	defer b.bambuMutex.Unlock()
	return b.bambuClients[printerID]
}

// StartBambuClients opens the MQTT connection for every configured Bambu printer
// without waiting for a monitor cycle to do it. A Bambu printer has no status
// endpoint to poll, so until its client is connected and has received a report
// the dashboard has nothing to show but offline; starting the connections at
// boot (and on every config reload) shrinks that window from a poll interval to
// the length of one TLS handshake.
//
// Each printer connects in its own goroutine so an unreachable one cannot hold
// up the others.
func (b *FilamentBridge) StartBambuClients() {
	cfg := b.GetConfigSnapshot()
	if cfg == nil {
		return
	}
	for printerID, pc := range cfg.Printers {
		if printerID == "no_printers" || pc.Type != PrinterTypeBambu || pc.Serial == "" {
			continue
		}
		go b.ensureBambuClient(printerID, pc)
	}
}

// retireStaleBambuClients disconnects and drops the clients of printers that are
// no longer configured (or are no longer Bambu printers), so a deleted printer
// does not leave an MQTT session open for the life of the process. Printers
// whose settings merely changed are rebuilt by ensureBambuClient.
func (b *FilamentBridge) retireStaleBambuClients(printers map[string]PrinterConfig) {
	b.bambuMutex.Lock()
	defer b.bambuMutex.Unlock()

	for printerID, bc := range b.bambuClients {
		if pc, ok := printers[printerID]; ok && pc.Type == PrinterTypeBambu {
			continue
		}
		bc.disconnect()
		delete(b.bambuClients, printerID)
		b.forgetPrinterStatus(printerID)
	}
}

// CloseBambuClients disconnects every Bambu MQTT session, for shutdown.
func (b *FilamentBridge) CloseBambuClients() {
	b.bambuMutex.Lock()
	defer b.bambuMutex.Unlock()

	for printerID, bc := range b.bambuClients {
		bc.disconnect()
		delete(b.bambuClients, printerID)
	}
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

// bambuSystemJobFiles names gcode the printer's firmware runs on its own, such
// as calibration routines. Over MQTT these report as ordinary jobs, but they are
// not prints: the file lives in firmware rather than on the SD card, so there is
// no sliced .3mf to read usage from, and tracking one only ends in a "no
// filament usage data" banner the user has no way to act on.
var bambuSystemJobFiles = map[string]bool{
	"auto_cali_for_user_param.gcode":       true, // flow dynamics calibration (A1)
	"new_machine_auto_cali_for_user.gcode": true, // first-run calibration (X2D)
}

// bambuIsSystemJob reports whether gcodeFile is a firmware routine rather than a
// user's print. Matched on the base name, case-insensitively, since firmware may
// report the file with or without a directory.
func bambuIsSystemJob(gcodeFile string) bool {
	return bambuSystemJobFiles[strings.ToLower(path.Base(gcodeFile))]
}

// bambuDashboardState maps Bambu's gcode_state onto the state vocabulary the
// rest of FilaBridge speaks, so a Bambu printer's badge reads the same as a
// PrusaLink one. PREPARE counts as printing: the job is already in flight.
func bambuDashboardState(state string) string {
	switch state {
	case bambuStateRunning, bambuStatePrepare:
		return StatePrinting
	case bambuStatePause:
		return StatePaused
	case bambuStateFinish:
		return StateFinished
	case bambuStateFailed:
		return StateError
	default: // IDLE, or anything the printer reports that we do not model
		return StateIdle
	}
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

// bambuAttributeUsage turns slice_info's per-filament grams into per-toolhead
// grams. When the printer reports which AMS tray each filament was loaded from,
// that decides the toolhead: tray N is toolhead N, and the external spool holder
// is the last configured toolhead. Anything the mapping cannot place cleanly
// falls back to the positional filament id - 1 every printer got before the
// mapping was read, so a printer the mapping does not fit behaves as it always
// has. The second result reports whether the mapping was used.
//
// Interim: "external is the last toolhead" stands in for real filament position
// identities, which replace the flat toolhead index entirely.
func bambuAttributeUsage(byFilament map[int]float64, mapping []int, toolheads int) (map[int]float64, bool) {
	if out, ok := bambuUsageByMapping(byFilament, mapping, toolheads); ok {
		return out, true
	}
	return bambuToToolheadUsage(byFilament), false
}

// bambuUsageByMapping places each filament by its mapping entry, and refuses
// (ok false) unless every filament lands on its own configured toolhead with the
// AMS trays below the external slot. That rules out a single toolhead printer,
// where there is nowhere else to put it, two external spools on a dual nozzle
// machine, which the flat index cannot tell apart, and trays from a second AMS
// beyond the configured count.
func bambuUsageByMapping(byFilament map[int]float64, mapping []int, toolheads int) (map[int]float64, bool) {
	if len(mapping) == 0 || toolheads <= 1 {
		return nil, false
	}
	external := toolheads - 1
	out := make(map[int]float64, len(byFilament))
	for id, grams := range byFilament {
		if id < 1 || id > len(mapping) {
			return nil, false
		}
		var toolhead int
		switch src := mapping[id-1]; {
		case src == bambuExternalSpool:
			toolhead = external
		case src >= 0 && src < external:
			toolhead = src
		default:
			return nil, false
		}
		if _, taken := out[toolhead]; taken {
			return nil, false
		}
		out[toolhead] = grams
	}
	return out, true
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

// bambuJobRef is what finding and attributing a job's filament usage needs from
// the printer's report.
type bambuJobRef struct {
	GcodeFile   string         // the project file on A1-class printers, an internal plate path on the X2D
	SubtaskName string         // the project's title, only ever a hint at the filename
	PlateIdx    int            // 0 when not reported: use the first plate
	Mapping     []int          // AMS tray per slicer filament, nil when not reported
	Layers      int            // the plate's layer count, which identifies the file
	Sources     map[int]string // mapping value -> material loaded there
}

func bambuJobRefFrom(p bambuPrint) bambuJobRef {
	return bambuJobRef{
		GcodeFile:   p.GcodeFile,
		SubtaskName: p.SubtaskName,
		PlateIdx:    p.PlateIdx,
		Mapping:     p.Mapping,
		Layers:      p.TotalLayerNum,
		Sources:     p.Sources,
	}
}

// bambuFilamentUsageFromFile finds the sliced file this job is printing and
// returns slice_info.config's grams per slicer filament (1-based ids, not
// toolheads). bambu_files.go covers how the file is identified.
func bambuFilamentUsageFromFile(ip, accessCode string, job bambuJobRef, idx *bambuFileIndex) (map[int]float64, error) {
	return bambuFilamentUsageFromFilePort(ip, bambuFTPSPort, accessCode, job, idx)
}

// bambuFilamentUsageFromFilePort is bambuFilamentUsageFromFile against an
// explicit port, so tests can point it at a fake printer.
func bambuFilamentUsageFromFilePort(ip string, port int, accessCode string, job bambuJobRef, idx *bambuFileIndex) (map[int]float64, error) {
	file, err := bambuFindSlicedFile(ip, port, accessCode, job, idx)
	if err != nil {
		return nil, err
	}
	log.Printf("Bambu: this print is %s (%d layers, grams %v)", file.Path, file.Layers, file.Usage)
	return file.Usage, nil
}

// fetchBambuUsage downloads the sliced .3mf over FTPS and returns per-toolhead
// filament grams from slice_info.config.
func (b *FilamentBridge) fetchBambuUsage(config PrinterConfig, job bambuJobRef) (map[int]float64, error) {
	byFilament, err := bambuFilamentUsageFromFile(config.IPAddress, config.APIKey, job, b.bambuFiles)
	if err != nil {
		return nil, err
	}
	usage, mapped := bambuAttributeUsage(byFilament, job.Mapping, config.Toolheads)
	if len(job.Mapping) > 0 && !mapped {
		log.Printf("Bambu %s: AMS mapping %v does not fit %d configured toolhead(s), attributing filament by position instead", config.Name, job.Mapping, config.Toolheads)
	}
	return usage, nil
}

// monitorBambu monitors a single Bambu printer for one poll cycle. It mirrors
// monitorPrusaLink over the MQTT-cached state: while printing it persists an
// in-flight job and captures the slicer's filament estimate (from the sliced
// .3mf over FTPS); on a terminal state it records usage against Spoolman,
// scaling a cancelled/failed print by the progress seen. State is read from the
// persistent MQTT client's cache.
func (b *FilamentBridge) monitorBambu(printerID string, config PrinterConfig) error {
	// Every exit below records the printer's state in the shared status cache:
	// there is no PrusaLink endpoint to fall back on, so the cache is the only
	// way the dashboard and status broadcasts learn what a Bambu printer is doing.
	if config.Serial == "" {
		err := fmt.Errorf("no serial configured")
		b.noteConnectivity(printerID, config.IPAddress, config.Name, err)
		b.cachePrinterStatus(printerID, StateOffline, err)
		return nil
	}
	client := b.ensureBambuClient(printerID, config)
	if !client.isConnected() {
		err := fmt.Errorf("MQTT not connected")
		b.noteConnectivity(printerID, config.IPAddress, config.Name, err)
		b.cachePrinterStatus(printerID, StateOffline, err)
		return nil
	}
	b.noteConnectivity(printerID, config.IPAddress, config.Name, nil)

	report, ok := client.snapshot()
	if !ok {
		// Session is open but the printer has not reported yet. Idle would be a
		// guess, and a wrong guess here reads as "printer is fine and doing
		// nothing" - which is indistinguishable from a printer that is actually
		// gone. Offline is the honest answer for a state we do not know.
		// onConnect requests a full state push, so this window closes as soon as
		// the first report lands, well inside one poll interval.
		b.cachePrinterStatus(printerID, StateOffline, nil)
		return nil
	}
	p := report.Print
	state := p.GcodeState
	jobName := bambuJobName(report)
	currentFile := p.GcodeFile

	b.cachePrinterStatus(printerID, bambuDashboardState(state), nil)

	// Give the printer its filament positions from what it says it has. Only
	// when that answer changed, since an X2D repeats its full state every few
	// seconds.
	b.syncBambuPositions(printerID, client)

	active, err := b.getActiveJob(printerID)
	if err != nil {
		log.Printf("Warning: failed to read active job for %s: %v", printerID, err)
	}

	b.noteStateChange(printerID, config.Name, state, jobName)

	switch {
	case bambuStateIsPrinting(state) && bambuIsSystemJob(currentFile):
		// A firmware routine such as calibration. Leave it untracked: with no
		// active job there are no FTPS fetches while it runs, no runout check
		// against an estimate that does not exist, and nothing to record (or
		// fail to record) when it ends. The dashboard still reads Printing,
		// which is accurate, since the printer is busy.

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
			if usage, err := b.fetchBambuUsage(config, bambuJobRefFrom(p)); err != nil {
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

		// With the estimate in hand, warn (and optionally pause) if the mapped
		// spool has less filament remaining than the print still needs. Same
		// path PrusaLink uses; the MQTT client is the pauser here.
		b.checkRunoutWarnings(printerID, config, client, aj)

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
	var fetchErr error
	if len(usage) == 0 {
		// Estimate was never captured while printing; try once more now. The
		// report still carries the job's name, plate and mapping, but it clears
		// gcode_file at the end, so the tracked filename stands in for it.
		job := bambuJobRef{GcodeFile: filename}
		if bc := b.existingBambuClient(active.PrinterID); bc != nil {
			if r, ok := bc.snapshot(); ok {
				job = bambuJobRefFrom(r.Print)
				job.GcodeFile = filename
			}
		}
		if u, err := b.fetchBambuUsage(config, job); err != nil {
			fetchErr = err
			log.Printf("Warning: end-of-print estimate fetch failed for %s: %v", printerName, err)
		} else {
			usage = u
		}
	}
	if len(usage) == 0 {
		if usageScale == 0 {
			return nil // nothing printed and no estimate: nothing to record
		}
		// Carry the underlying reason into the dashboard banner: "no usage
		// data" alone sends the user to the logs to learn whether the file
		// was missing, the download failed, or the parse came up empty.
		msg := bambuMissingEstimateError(fetchErr)
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

// bambuMissingEstimateError explains a print that ended with no filament
// estimate. The sliced file is the only place per-filament grams exist, so a
// print whose file cannot be read is not recorded at all. The likeliest cause on
// an X2-series printer is a print started from internal storage: FTPS serves
// only the removable drive, and the report names no file, so there is nothing to
// read. The dashboard already asks the user to update Spoolman by hand, so this
// says why rather than what to do. The underlying error is carried along when
// there is one, since "not found" and "the download failed" need different fixes.
func bambuMissingEstimateError(fetchErr error) string {
	msg := "could not read the sliced file for this print, so no filament was recorded. " +
		"Prints started from the printer's internal storage cannot be tracked, " +
		"send them to the removable drive instead"
	if fetchErr != nil {
		msg = fmt.Sprintf("%s (%v)", msg, fetchErr)
	}
	return msg
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

// bambuFTPSDialFunc builds the dial function used for BOTH the control and the
// data connection of a Bambu FTPS session. It does two things the default
// dialer cannot:
//
//   - Implicit TLS: every connection (control on 990 and each passive data
//     connection) is wrapped in TLS immediately, with verification skipped for
//     the printer's self-signed LAN certificate.
//   - Passive-address pinning: A1/P1 firmware does not answer EPSV, so the
//     client falls back to PASV - and the printer replies "227 Entering Passive
//     Mode (0,0,0,0,p1,p2)". Dialing 0.0.0.0 is refused, which kills every LIST
//     and RETR. The data connection always belongs on the printer's own
//     address, so ignore the advertised host and keep only the port (the same
//     thing curl does with --ftp-skip-pasv-ip).
func bambuFTPSDialFunc(ip string, tlsConfig *tls.Config) func(network, address string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: bambuFTPSTimeout}
	return func(network, address string) (net.Conn, error) {
		pinned, err := bambuFTPSDataAddr(ip, address)
		if err != nil {
			return nil, err
		}
		conn, err := dialer.Dial(network, pinned)
		if err != nil {
			return nil, err
		}
		return tls.Client(conn, tlsConfig), nil
	}
}

// bambuFTPSDataAddr keeps the port the printer advertised and swaps in the
// printer's own host, so a PASV reply of "(0,0,0,0,p1,p2)" still lands on the
// printer.
func bambuFTPSDataAddr(ip, advertised string) (string, error) {
	_, port, err := net.SplitHostPort(advertised)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(ip, port), nil
}

// dialBambuFTPS opens and authenticates an implicit-TLS FTPS session to a Bambu
// printer (port 990, user "bblp", pass = access code).
func dialBambuFTPS(ip, accessCode string) (*ftp.ServerConn, error) {
	return dialBambuFTPSPort(ip, bambuFTPSPort, accessCode)
}

// dialBambuFTPSPort is dialBambuFTPS with the control port spelled out, so a
// test can point it at a fake printer on an ephemeral port.
func dialBambuFTPSPort(ip string, port int, accessCode string) (*ftp.ServerConn, error) {
	// One config shared by the control and data connections: the printer's
	// certificate is self-signed (LAN-only, still encrypted), and a shared
	// session cache lets the data connection resume the control connection's
	// TLS session, which some Bambu firmware requires.
	tlsConfig := &tls.Config{ // #nosec G402 - self-signed LAN device
		InsecureSkipVerify: true,
		ServerName:         ip, // stable session-cache key across both ports
		ClientSessionCache: tls.NewLRUClientSessionCache(4),
	}
	// Both options together, as the library documents: the dial func makes the
	// connections, while DialWithTLS is what makes Login send "PBSZ 0"/"PROT P"
	// so the printer expects an encrypted data channel.
	conn, err := ftp.Dial(net.JoinHostPort(ip, strconv.Itoa(port)),
		ftp.DialWithTimeout(bambuFTPSTimeout),
		ftp.DialWithTLS(tlsConfig),
		ftp.DialWithDialFunc(bambuFTPSDialFunc(ip, tlsConfig)),
	)
	if err != nil {
		return nil, fmt.Errorf("FTPS dial: %w", err)
	}
	if err := conn.Login(bambuMQTTUser, accessCode); err != nil {
		_ = conn.Quit()
		return nil, fmt.Errorf("FTPS login: %w", err)
	}
	return conn, nil
}

// fetchBambuFile downloads a file from a Bambu printer over implicit-TLS FTPS.
func fetchBambuFile(ip, accessCode, remotePath string) ([]byte, error) {
	return fetchBambuFilePort(ip, bambuFTPSPort, accessCode, remotePath)
}

// fetchBambuFilePort is fetchBambuFile against an explicit port, so tests can
// point it at a fake printer.
func fetchBambuFilePort(ip string, port int, accessCode, remotePath string) ([]byte, error) {
	conn, err := dialBambuFTPSPort(ip, port, accessCode)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Quit() }()

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
//
// A1-class printers report the project file itself as gcode_file. The X2D
// reports an internal plate path instead (/data/Metadata/plate_1.gcode) that
// FTPS cannot reach, and keeps the project file on the SD card named after the
// job. Both names are tried, whichever looks like the project file first.
func bambuSlicedFileCandidates(gcodeFile, subtaskName string) []string {
	fromFile := strings.TrimPrefix(gcodeFile, "/")
	fromJob := ""
	if subtaskName != "" {
		fromJob = subtaskName + ".gcode.3mf"
	}
	names := []string{fromFile, fromJob}
	if !strings.HasSuffix(strings.ToLower(fromFile), ".3mf") {
		names = []string{fromJob, fromFile}
	}
	var out []string
	seen := make(map[string]bool)
	for _, name := range names {
		if name == "" {
			continue
		}
		for _, remote := range []string{
			bambuCacheDir + "/" + name, // cache/foo.gcode.3mf
			name,                       // foo.gcode.3mf (root)
		} {
			if !seen[remote] {
				seen[remote] = true
				out = append(out, remote)
			}
		}
	}
	return out
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
	usage, _, err := parseSliceInfoPlate(threemf, plateIndex)
	return usage, err
}

// parseSliceInfoPlate reads a plate's per-filament grams and the material each
// filament was sliced for. The material is what tells two slices of one model
// apart when they differ only by filament, which a filename cannot be trusted
// to say.
func parseSliceInfoPlate(threemf []byte, plateIndex int) (map[int]float64, map[int]string, error) {
	xmlData, err := extractSliceInfoXML(threemf)
	if err != nil {
		return nil, nil, err
	}
	var info bambuSliceInfo
	if err := xml.Unmarshal(xmlData, &info); err != nil {
		return nil, nil, fmt.Errorf("parse slice_info: %w", err)
	}
	if len(info.Plates) == 0 {
		return nil, nil, fmt.Errorf("no plates in slice_info")
	}

	plate := &info.Plates[0]
	if plateIndex > 0 {
		found := false
		for i := range info.Plates {
			if info.Plates[i].index() == plateIndex {
				plate = &info.Plates[i]
				found = true
				break
			}
		}
		if !found {
			log.Printf("Warning: %s has no plate %d, using the first plate", sliceInfoPath, plateIndex)
		}
	}

	usage := make(map[int]float64)
	types := make(map[int]string)
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
		types[id] = strings.TrimSpace(fil.Type)
	}
	return usage, types, nil
}

// bambuCaptureBytes bounds how much of a cached plate gcode the capture reads.
// The slicer's summary block sits at the very top of the file, and the printer
// writes these files progressively while it prepares a print, so reading a
// prefix is both sufficient and safe on a file that is still growing.
const bambuCaptureBytes = 96 * 1024

// fetchBambuFilePrefix downloads at most limit bytes of a file over FTPS, for
// large files whose interesting content is at the top.
func fetchBambuFilePrefix(ip, accessCode, remotePath string, limit int64) ([]byte, error) {
	return fetchBambuFilePrefixPort(ip, bambuFTPSPort, accessCode, remotePath, limit)
}

// fetchBambuFilePrefixPort is fetchBambuFilePrefix with the control port spelled
// out, so a test can point it at a fake printer on an ephemeral port.
func fetchBambuFilePrefixPort(ip string, port int, accessCode, remotePath string, limit int64) ([]byte, error) {
	conn, err := dialBambuFTPSPort(ip, port, accessCode)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Quit() }()

	r, err := conn.Retr(remotePath)
	if err != nil {
		return nil, fmt.Errorf("FTPS retrieve %q: %w", remotePath, err)
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(io.LimitReader(r, limit))
}

// gcodeCommentLines returns the ";" comment lines from a gcode prefix, which is
// where Bambu writes its per-print summary (filament weight, length, colour)
// and the full slicer configuration. Long lines are truncated: a single
// change_filament_gcode value runs to several KB on one line.
func gcodeCommentLines(gcode []byte, maxLines, maxLen int) []string {
	out := make([]string, 0, maxLines)
	for _, raw := range strings.Split(string(gcode), "\n") {
		line := strings.TrimRight(raw, "\r")
		if !strings.HasPrefix(line, ";") {
			continue
		}
		if len(line) > maxLen {
			line = line[:maxLen] + " ...(truncated)"
		}
		out = append(out, line)
		if len(out) >= maxLines {
			break
		}
	}
	return out
}

// prettyJSON re-indents a JSON payload for a human reading a capture log.
func prettyJSON(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// The three kinds of file a Bambu print leaves on the SD card that are worth
// capturing, as observed on an A1: "1_Cable Wheels.gcode.bbl" (the job
// descriptor, carrying the AMS mapping), "Cable Wheels_plate_1.gcode" (the
// extracted plate the printer actually executes) and "Cable Wheels.gcode.3mf"
// (the project as sliced).
const (
	captureKindJobDescriptor = "bbl"
	captureKindPlateGcode    = "plate"
	captureKindProject       = "3mf"
)

// bambuCaptureKind classifies an SD-card filename, or returns "" for a name the
// capture has no use for. Kept separate from the directory walk so the rules
// can be pinned against real filenames in a test: getting this wrong means a
// capture run silently skips the file it was meant to collect.
func bambuCaptureKind(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".bbl"):
		return captureKindJobDescriptor
	case strings.HasSuffix(lower, ".gcode") && strings.Contains(lower, "_plate_"):
		return captureKindPlateGcode
	case strings.HasSuffix(lower, ".3mf"):
		return captureKindProject
	}
	return ""
}

// captureBambuFTPS is the FTPS half of the developer probe. It captures, in one
// pass, everything a Bambu print leaves on the SD card:
//
//   - the root and cache directory listings
//   - every job descriptor (*.bbl), verbatim. This is where the printer records
//     the slicer's AMS mapping and the true source path of the project file.
//   - the comment header of every cached plate gcode. That file is what the
//     printer actually executes, so its "total filament weight [g]" is the
//     authoritative usage for the plate being printed, whichever plate that is.
//   - slice_info.config from each project .3mf, with per-plate grams, so the
//     two sources can be compared.
//
// Nothing here depends on there being an active job: an idle printer still has
// AMS state and a populated cache, which is the whole point when capturing from
// a printer we are not free to start a print on.
func captureBambuFTPS(ip, accessCode, gcodeFile string) {
	log.Printf("=== FTPS capture (%s:%d) ===", ip, bambuFTPSPort)
	conn, err := dialBambuFTPS(ip, accessCode)
	if err != nil {
		log.Printf("FTPS: %v", err)
		return
	}

	var bblFiles, plateFiles, threeMFs []string
	for _, dir := range []string{"/", "/" + bambuCacheDir} {
		entries, err := conn.List(dir)
		if err != nil {
			log.Printf("LIST %s: %v", dir, err)
			continue
		}
		log.Printf("--- LIST %s ---", dir)
		for _, e := range entries {
			log.Printf("  %10d  %s", e.Size, e.Name)
			full := strings.TrimPrefix(strings.TrimSuffix(dir, "/")+"/"+e.Name, "/")
			switch bambuCaptureKind(e.Name) {
			case captureKindJobDescriptor:
				bblFiles = append(bblFiles, full)
			case captureKindPlateGcode:
				plateFiles = append(plateFiles, full)
			case captureKindProject:
				threeMFs = append(threeMFs, full)
			}
		}
	}
	_ = conn.Quit()

	// The project file named by MQTT goes first, so a slow run that gets cut
	// short still captured the one belonging to the current job.
	if gcodeFile != "" {
		want := path.Base(gcodeFile)
		sort.SliceStable(threeMFs, func(i, _ int) bool { return path.Base(threeMFs[i]) == want })
	}

	for _, p := range bblFiles {
		data, err := fetchBambuFile(ip, accessCode, p)
		if err != nil {
			log.Printf("job descriptor %s: %v", p, err)
			continue
		}
		log.Printf("--- job descriptor %s (%d bytes) ---\n%s", p, len(data), prettyJSON(data))
	}

	for _, p := range plateFiles {
		data, err := fetchBambuFilePrefix(ip, accessCode, p, bambuCaptureBytes)
		if err != nil {
			log.Printf("plate gcode %s: %v", p, err)
			continue
		}
		log.Printf("--- plate gcode %s (first %d bytes) ---", p, len(data))
		for _, line := range gcodeCommentLines(data, 400, 300) {
			log.Printf("  %s", line)
		}
	}

	for i, p := range threeMFs {
		if i >= 2 {
			log.Printf("(%d further .3mf files left undownloaded)", len(threeMFs)-i)
			break
		}
		data, err := fetchBambuFile(ip, accessCode, p)
		if err != nil {
			log.Printf("project file %s: %v", p, err)
			continue
		}
		xmlData, err := extractSliceInfoXML(data)
		if err != nil {
			log.Printf("project file %s (%d bytes): %v", p, len(data), err)
			continue
		}
		log.Printf("--- slice_info.config from %s (%d bytes) ---\n%s", p, len(data), string(xmlData))

		var info bambuSliceInfo
		if err := xml.Unmarshal(xmlData, &info); err != nil {
			log.Printf("  parse: %v", err)
			continue
		}
		for j := range info.Plates {
			idx := info.Plates[j].index()
			usage, err := parseSliceInfoUsage(data, idx)
			if err != nil {
				log.Printf("  plate %d: %v", idx, err)
				continue
			}
			log.Printf("  plate %d -> per-filament grams %v", idx, usage)
		}
	}
}

// runBambuProbe is a developer tool (main -bambu-probe): a one-shot capture of
// everything FilaBridge can learn about a Bambu printer over the LAN. It prints
// the live MQTT state (including the full AMS block), then hands off to
// captureBambuFTPS for what is on the SD card.
//
// It is read-only in every meaningful sense: it subscribes to the report topic,
// publishes one "pushall" asking the printer to describe itself, and reads
// files. It never starts, pauses, or stops a print, which matters when the
// printer belongs to someone else.
func runBambuProbe(ip, serial, code string, dur time.Duration) {
	bambuDebugRaw = true
	log.Printf("=== Bambu capture: %s (serial %s) ===", ip, serial)
	bc := newBambuClient(ip, serial, code)
	if err := bc.connect(); err != nil {
		log.Fatalf("connect failed: %v (check the IP, serial and access code, and that the printer is in LAN Mode)", err)
	}
	defer bc.disconnect()

	// Wait for the first report, which the pushall usually answers in well
	// under a second, then briefly longer for the complete state push carrying
	// the AMS block. Deliberately not waiting for an active job: an idle
	// printer is still worth capturing.
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		if _, ok := bc.snapshot(); ok {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	grace := time.Now().Add(5 * time.Second)
	for time.Now().Before(grace) && len(bc.fullReportJSON()) == 0 {
		time.Sleep(200 * time.Millisecond)
	}

	report, ok := bc.snapshot()
	if !ok {
		log.Printf("no MQTT reports received within %s. Check the serial number and that LAN Mode is enabled.", dur)
		return
	}
	log.Printf("=== MQTT state ===")
	log.Printf("state=%q job=%q progress=%d%% layer=%d/%d file=%q",
		report.Print.GcodeState, bambuJobName(report), report.Print.McPercent,
		report.Print.LayerNum, report.Print.TotalLayerNum, report.Print.GcodeFile)
	if full := bc.fullReportJSON(); len(full) > 0 {
		log.Printf("--- full state push ---\n%s", prettyJSON(full))
	} else {
		log.Printf("no complete state push seen; only the raw deltas above are available")
	}

	captureBambuFTPS(ip, code, report.Print.GcodeFile)
	log.Printf("=== capture complete ===")
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
				job := bambuJobRefFrom(r.Print)
				job.GcodeFile = lastFile
				usage, err := bambuFilamentUsageFromFile(ip, code, job, newBambuFileIndex())
				if err != nil {
					log.Printf("Bambu watch: estimate fetch failed: %v", err)
				} else {
					log.Printf("Bambu watch: per-filament grams %v, AMS mapping %v (status=%s)", usage, job.Mapping, map[bool]string{true: "completed", false: "cancelled"}[completed])
				}
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	log.Printf("Bambu watch: deadline reached without a terminal transition (last state %q, %d%%)", lastState, lastPct)
}
