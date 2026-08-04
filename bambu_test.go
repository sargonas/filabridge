package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// makeThreeMF wraps slice_info XML in a minimal .3mf (zip) for parser tests.
func makeThreeMF(t *testing.T, sliceInfoXML string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(sliceInfoPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(sliceInfoXML)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestParseSliceInfoUsage uses the exact slice_info.config captured from a live
// A1 print, so the parser stays pinned to the real Bambu format.
func TestParseSliceInfoUsage(t *testing.T) {
	const realXML = `<?xml version="1.0" encoding="UTF-8"?>
<config>
  <header>
    <header_item key="X-BBL-Client-Type" value="slicer"/>
  </header>
  <plate>
    <metadata key="index" value="1"/>
    <metadata key="weight" value="24.15"/>
    <object identify_id="453" name="Grid 4x5.stl" skipped="false" />
    <filament id="1" tray_info_idx="GFSNL04" type="PLA" color="#00AE42" used_m="8.30" used_g="24.15" group_id="0"/>
  </plate>
</config>`
	usage, err := parseSliceInfoUsage(makeThreeMF(t, realXML), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[1] != 24.15 {
		t.Fatalf("want map[1:24.15], got %v", usage)
	}
}

// TestParseSliceInfoUsageMultiPlate covers plate selection and skipping unused
// (0 g) filament slots on a multi-material, multi-plate file.
func TestParseSliceInfoUsageMultiPlate(t *testing.T) {
	const xml = `<config>
  <plate>
    <metadata key="index" value="1"/>
    <filament id="1" type="PLA" used_g="10.0"/>
  </plate>
  <plate>
    <metadata key="index" value="2"/>
    <filament id="1" type="PLA" used_g="5.5"/>
    <filament id="2" type="PETG" used_g="0.00"/>
    <filament id="3" type="ABS" used_g="7.25"/>
  </plate>
</config>`

	plate2, err := parseSliceInfoUsage(makeThreeMF(t, xml), 2)
	if err != nil {
		t.Fatal(err)
	}
	if plate2[1] != 5.5 || plate2[3] != 7.25 {
		t.Fatalf("plate 2 grams wrong: %v", plate2)
	}
	if _, ok := plate2[2]; ok {
		t.Errorf("unused 0 g filament must be skipped: %v", plate2)
	}

	// plateIndex 0 selects the first plate.
	first, err := parseSliceInfoUsage(makeThreeMF(t, xml), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[1] != 10.0 {
		t.Fatalf("default plate wrong: %v", first)
	}
}

// fakeMQTTMessage is a minimal mqtt.Message carrying only a payload, for
// exercising onMessage without a broker.
type fakeMQTTMessage struct{ payload []byte }

func (m fakeMQTTMessage) Duplicate() bool   { return false }
func (m fakeMQTTMessage) Qos() byte         { return 0 }
func (m fakeMQTTMessage) Retained() bool    { return false }
func (m fakeMQTTMessage) Topic() string     { return "" }
func (m fakeMQTTMessage) MessageID() uint16 { return 0 }
func (m fakeMQTTMessage) Payload() []byte   { return m.payload }
func (m fakeMQTTMessage) Ack()              {}

// TestBambuReportPartialMerge verifies the core assumption behind the state
// cache: Bambu emits partial reports, and unmarshaling each into the same struct
// must update only the fields present, leaving the rest intact.
func TestBambuReportPartialMerge(t *testing.T) {
	bc := &bambuClient{serial: "TEST"}

	bc.onMessage(nil, fakeMQTTMessage{[]byte(`{"print":{"gcode_state":"RUNNING","subtask_name":"benchy","gcode_file":"benchy.3mf","mc_percent":10}}`)})
	got, ok := bc.snapshot()
	if !ok {
		t.Fatal("expected haveReport true after first message")
	}
	if got.Print.GcodeState != bambuStateRunning || got.Print.SubtaskName != "benchy" || got.Print.McPercent != 10 {
		t.Fatalf("initial report not parsed: %+v", got.Print)
	}

	// Partial update: only progress. State/name/file must survive.
	bc.onMessage(nil, fakeMQTTMessage{[]byte(`{"print":{"mc_percent":55}}`)})
	got, _ = bc.snapshot()
	if got.Print.McPercent != 55 {
		t.Errorf("mc_percent not updated: %d", got.Print.McPercent)
	}
	if got.Print.GcodeState != bambuStateRunning || got.Print.SubtaskName != "benchy" || got.Print.GcodeFile != "benchy.3mf" {
		t.Errorf("partial update clobbered prior fields: %+v", got.Print)
	}

	// Terminal transition keeps the job name for end-of-print handling.
	bc.onMessage(nil, fakeMQTTMessage{[]byte(`{"print":{"gcode_state":"FINISH","mc_percent":100}}`)})
	got, _ = bc.snapshot()
	if got.Print.GcodeState != bambuStateFinish || got.Print.McPercent != 100 || got.Print.SubtaskName != "benchy" {
		t.Errorf("terminal update wrong: %+v", got.Print)
	}
}

func TestBambuToToolheadUsage(t *testing.T) {
	// slice_info filament ids are 1-based; toolheads are 0-based.
	got := bambuToToolheadUsage(map[int]float64{1: 24.15, 3: 7.0})
	if got[0] != 24.15 || got[2] != 7.0 {
		t.Fatalf("1-based -> 0-based mapping wrong: %v", got)
	}
	if _, ok := got[1]; ok {
		t.Errorf("filament id 1 should map to toolhead 0, not 1: %v", got)
	}
}

func TestBambuJobID(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	a := bambuJobID("benchy.gcode.3mf", start)
	b := bambuJobID("benchy.gcode.3mf", start)
	if a != b {
		t.Errorf("job id must be stable for same inputs: %d != %d", a, b)
	}
	if a == 0 {
		t.Error("job id must never be 0 (the no-dedupe sentinel)")
	}
	if bambuJobID("benchy.gcode.3mf", start.Add(time.Second)) == a {
		t.Error("different start time should yield a different job id")
	}
	if bambuJobID("other.gcode.3mf", start) == a {
		t.Error("different filename should yield a different job id")
	}
}

func TestBambuStatePredicates(t *testing.T) {
	for _, s := range []string{bambuStateRunning, bambuStatePause, bambuStatePrepare} {
		if !bambuStateIsPrinting(s) {
			t.Errorf("%s should be printing", s)
		}
		if bambuStateIsTerminal(s) {
			t.Errorf("%s should not be terminal", s)
		}
	}
	for _, s := range []string{bambuStateFinish, bambuStateFailed, bambuStateIdle} {
		if !bambuStateIsTerminal(s) {
			t.Errorf("%s should be terminal", s)
		}
		if bambuStateIsPrinting(s) {
			t.Errorf("%s should not be printing", s)
		}
	}
}

func TestBambuDashboardState(t *testing.T) {
	cases := map[string]string{
		bambuStateRunning: StatePrinting,
		bambuStatePrepare: StatePrinting,
		bambuStatePause:   StatePaused,
		bambuStateFinish:  StateFinished,
		bambuStateFailed:  StateError,
		bambuStateIdle:    StateIdle,
		"":                StateIdle,
		"SOMETHING_NEW":   StateIdle,
	}
	for state, want := range cases {
		if got := bambuDashboardState(state); got != want {
			t.Errorf("bambuDashboardState(%q) = %q, want %q", state, got, want)
		}
	}
}

// TestBambuStatusComesFromCacheNotPrusaLink: a Bambu printer's dashboard state
// is served from the status cache the MQTT monitor fills, and it is never polled
// over PrusaLink. The Bambu printer here points at a working PrusaLink server on
// purpose, so a regression that polls it would report that server's IDLE instead
// of the offline/cached answer.
func TestBambuStatusComesFromCacheNotPrusaLink(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	b := newTestBridge(t, printer, spoolman)

	if err := b.SavePrinterConfig("printer_bambu", PrinterConfig{
		Name: "X1C", IPAddress: printer.Addr(), APIKey: "accesscode", Toolheads: 4,
		Type: PrinterTypeBambu, Serial: "01S00A1234567890",
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

	// Nothing cached yet, so offline rather than the IDLE the PrusaLink fake
	// would happily answer with.
	status, err := b.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	if got := status.Printers["printer_bambu"].State; got != StateOffline {
		t.Fatalf("uncached Bambu state = %q, want %q", got, StateOffline)
	}

	// Once the monitor has cached MQTT state, that is what the dashboard shows.
	b.cachePrinterStatus("printer_bambu", StatePrinting, nil)
	status, err = b.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	if got := status.Printers["printer_bambu"].State; got != StatePrinting {
		t.Fatalf("cached Bambu state = %q, want %q", got, StatePrinting)
	}

	// The PrusaLink printer alongside it is still polled as usual.
	if got := status.Printers["printer_test"].State; got != StateIdle {
		t.Fatalf("PrusaLink printer state = %q, want %q", got, StateIdle)
	}
}

// stubToken is an mqtt.Token that has already completed, carrying an optional
// error, so publishes resolve synchronously in tests.
type stubToken struct{ err error }

func (t stubToken) Wait() bool                     { return true }
func (t stubToken) WaitTimeout(time.Duration) bool { return true }
func (t stubToken) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (t stubToken) Error() error { return t.err }

// stubMQTT is an mqtt.Client standing in for a printer's broker: it reports
// itself connected and records every publish, so command sending can be
// exercised without a network.
// The two connection predicates are modelled separately because paho draws the
// same distinction: IsConnected is also true while merely reconnecting, and only
// IsConnectionOpen means a live session.
type stubMQTT struct {
	mu         sync.Mutex
	open       bool // a live session
	retrying   bool // reconnect attempts in flight, nothing actually connected
	publishErr error
	topics     []string
	payloads   []string
}

func (s *stubMQTT) IsConnected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open || s.retrying
}
func (s *stubMQTT) IsConnectionOpen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open
}
func (s *stubMQTT) Connect() mqtt.Token { return stubToken{} }
func (s *stubMQTT) Disconnect(uint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open, s.retrying = false, false
}

// powerOff simulates the printer disappearing: paho keeps retrying in the
// background, so it still calls itself "connected" while nothing is open.
func (s *stubMQTT) powerOff() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open, s.retrying = false, true
}
func (s *stubMQTT) Publish(topic string, _ byte, _ bool, payload interface{}) mqtt.Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.topics = append(s.topics, topic)
	s.payloads = append(s.payloads, fmt.Sprint(payload))
	return stubToken{err: s.publishErr}
}
func (s *stubMQTT) Subscribe(string, byte, mqtt.MessageHandler) mqtt.Token { return stubToken{} }
func (s *stubMQTT) SubscribeMultiple(map[string]byte, mqtt.MessageHandler) mqtt.Token {
	return stubToken{}
}
func (s *stubMQTT) Unsubscribe(...string) mqtt.Token        { return stubToken{} }
func (s *stubMQTT) AddRoute(string, mqtt.MessageHandler)    {}
func (s *stubMQTT) OptionsReader() mqtt.ClientOptionsReader { return mqtt.ClientOptionsReader{} }
func (s *stubMQTT) sent() (topics []string, payloads []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.topics...), append([]string(nil), s.payloads...)
}

// stubBambuClient builds a bambuClient backed by a stub broker, already holding
// the given gcode_state and sliced filename.
func stubBambuClient(ip, serial, code, state, gcodeFile string) (*bambuClient, *stubMQTT) {
	stub := &stubMQTT{open: true}
	bc := &bambuClient{ip: ip, serial: serial, accessCode: code, client: stub}
	bc.onMessage(nil, fakeMQTTMessage{[]byte(fmt.Sprintf(
		`{"print":{"gcode_state":%q,"gcode_file":%q,"subtask_name":"bambu job","mc_percent":0}}`,
		state, gcodeFile))})
	return bc, stub
}

// bambuTestPrinter registers a Bambu printer on a test bridge alongside the
// PrusaLink one, hands it a stubbed MQTT client, and returns both.
func bambuTestPrinter(t *testing.T, b *FilamentBridge, state, gcodeFile string) (PrinterConfig, *stubMQTT) {
	t.Helper()
	const (
		printerID = "printer_bambu"
		ip        = "10.0.0.9"
		serial    = "01S00A1234567890"
		code      = "accesscode"
	)
	if err := b.SavePrinterConfig(printerID, PrinterConfig{
		Name: "X1C", IPAddress: ip, APIKey: code, Toolheads: 4,
		Type: PrinterTypeBambu, Serial: serial,
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

	bc, stub := stubBambuClient(ip, serial, code, state, gcodeFile)
	b.bambuMutex.Lock()
	b.bambuClients[printerID] = bc
	b.bambuMutex.Unlock()

	return b.GetConfigSnapshot().Printers[printerID], stub
}

// TestBambuPrintCommands: pause and resume go out on the printer's request
// topic with distinct sequence ids, and a disconnected client refuses to send
// rather than reporting a pause that never happened.
func TestBambuPrintCommands(t *testing.T) {
	bc, stub := stubBambuClient("10.0.0.9", "SERIAL1", "code", bambuStateRunning, "part.3mf")

	if err := bc.PauseJob(0); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	if err := bc.ResumeJob(0); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}

	topics, payloads := stub.sent()
	if len(payloads) != 2 {
		t.Fatalf("expected 2 publishes, got %d: %v", len(payloads), payloads)
	}
	for _, topic := range topics {
		if topic != "device/SERIAL1/request" {
			t.Errorf("command sent to %q, want the request topic", topic)
		}
	}
	if !strings.Contains(payloads[0], `"command":"pause"`) {
		t.Errorf("pause payload = %s", payloads[0])
	}
	if !strings.Contains(payloads[1], `"command":"resume"`) {
		t.Errorf("resume payload = %s", payloads[1])
	}
	if !strings.Contains(payloads[0], `"sequence_id":"1"`) || !strings.Contains(payloads[1], `"sequence_id":"2"`) {
		t.Errorf("sequence ids must increment: %v", payloads)
	}

	// IsPaused reads the cached report, no round trip.
	if paused, err := bc.IsPaused(); err != nil || paused {
		t.Errorf("IsPaused while RUNNING = %v, %v", paused, err)
	}
	bc.onMessage(nil, fakeMQTTMessage{[]byte(`{"print":{"gcode_state":"PAUSE"}}`)})
	if paused, err := bc.IsPaused(); err != nil || !paused {
		t.Errorf("IsPaused while PAUSE = %v, %v", paused, err)
	}

	// A session that is only reconnecting must not report a pause it could not
	// deliver, even though paho still calls itself connected.
	stub.powerOff()
	if err := bc.PauseJob(0); err == nil {
		t.Error("a reconnecting client must not claim to have paused the print")
	}
	stub.Disconnect(0)
	if err := bc.PauseJob(0); err == nil {
		t.Error("a disconnected client must not claim to have paused the print")
	}
	if _, payloads := stub.sent(); len(payloads) != 2 {
		t.Errorf("client with no live session still published: %v", payloads)
	}
}

// TestBambuLowFilamentWarningAndPause: the low-filament path is shared with
// PrusaLink, so a Bambu print whose mapped spool is short warns and (with the
// opt-in on) auto-pauses over MQTT, and acknowledging resumes it.
func TestBambuLowFilamentWarningAndPause(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	spoolman.Spools[7] = &fakeSpool{ID: 7, Name: "Nearly Empty", RemainingWeight: 40}
	b := newTestBridge(t, printer, spoolman)

	const gcodeFile = "cache/part.gcode.3mf"
	config, stub := bambuTestPrinter(t, b, bambuStateRunning, gcodeFile)
	if err := b.SetToolheadMapping("X1C", 0, 7); err != nil {
		t.Fatal(err)
	}
	if err := b.SetConfigValue(ConfigKeyRunoutPauseEnabled, "true"); err != nil {
		t.Fatal(err)
	}

	// Seed the slicer estimate the way a previous cycle's FTPS fetch would, so
	// the monitor has usage to compare without reaching for the printer.
	if err := b.upsertActiveJob(&activeJob{
		PrinterID: "printer_bambu", JobID: 4242, Filename: gcodeFile,
		StartedAt: time.Now(), Usage: map[int]float64{0: 372.68},
	}); err != nil {
		t.Fatal(err)
	}

	if err := b.monitorBambu("printer_bambu", config); err != nil {
		t.Fatalf("monitorBambu: %v", err)
	}

	warnings := b.GetRunoutWarnings()
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %+v", len(warnings), warnings)
	}
	w := warnings[0]
	if w.SpoolID != 7 || w.RequiredWeight != 372.68 || w.RemainingWeight != 40 {
		t.Errorf("warning fields: %+v", w)
	}
	if !w.AutoPaused {
		t.Fatal("auto-pause is enabled but the warning did not record a pause")
	}
	if _, payloads := stub.sent(); len(payloads) != 1 || !strings.Contains(payloads[0], `"command":"pause"`) {
		t.Fatalf("expected one pause command, got %v", payloads)
	}

	// The printer reports the pause; acknowledging the warning resumes it.
	bc := b.existingBambuClient("printer_bambu")
	bc.onMessage(nil, fakeMQTTMessage{[]byte(`{"print":{"gcode_state":"PAUSE"}}`)})
	if err := b.AcknowledgeRunoutWarning(w.ID); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	_, payloads := stub.sent()
	if len(payloads) != 2 || !strings.Contains(payloads[1], `"command":"resume"`) {
		t.Fatalf("acknowledge did not resume: %v", payloads)
	}

	// A print the user already resumed at the printer is left alone.
	bc.onMessage(nil, fakeMQTTMessage{[]byte(`{"print":{"gcode_state":"RUNNING"}}`)})
	b.warnMutex.Lock()
	w.Acknowledged = false
	b.runoutWarnings[w.ID] = w
	b.warnMutex.Unlock()
	if err := b.AcknowledgeRunoutWarning(w.ID); err != nil {
		t.Fatalf("second acknowledge: %v", err)
	}
	if _, payloads := stub.sent(); len(payloads) != 2 {
		t.Errorf("resumed a print that was not paused: %v", payloads)
	}
}

// TestBambuStatusBeforeFirstMonitorCycle: a Bambu printer has no endpoint to
// poll, so with nothing in the status cache the dashboard reads its MQTT
// client's own state rather than declaring the printer offline for a whole poll
// interval. With no client, or a client that has not heard from the printer
// yet, offline is still the honest answer.
func TestBambuStatusBeforeFirstMonitorCycle(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	b := newTestBridge(t, printer, spoolman)
	config, _ := bambuTestPrinter(t, b, bambuStateRunning, "part.3mf")

	bambuState := func(t *testing.T) string {
		t.Helper()
		status, err := b.GetStatus()
		if err != nil {
			t.Fatal(err)
		}
		return status.Printers["printer_bambu"].State
	}

	// Nothing cached, but the MQTT client is connected and has a report.
	if got := bambuState(t); got != StatePrinting {
		t.Errorf("state before first monitor cycle = %q, want %q", got, StatePrinting)
	}

	// A connected client that has not received a report yet stays offline.
	b.forgetPrinterStatus("printer_bambu")
	bare, _ := stubBambuClient(config.IPAddress, config.Serial, config.APIKey, "", "")
	bare.mu.Lock()
	bare.haveReport = false
	bare.mu.Unlock()
	b.bambuMutex.Lock()
	b.bambuClients["printer_bambu"] = bare
	b.bambuMutex.Unlock()
	if got := bambuState(t); got != StateOffline {
		t.Errorf("state with no report yet = %q, want %q", got, StateOffline)
	}

	// No client at all (web-only mode, printer never dialed): offline.
	b.forgetPrinterStatus("printer_bambu")
	b.bambuMutex.Lock()
	delete(b.bambuClients, "printer_bambu")
	b.bambuMutex.Unlock()
	if got := bambuState(t); got != StateOffline {
		t.Errorf("state with no MQTT client = %q, want %q", got, StateOffline)
	}
}

// TestBambuPoweredOffPrinterReadsOffline: a printer that has been switched off
// must read offline, not keep serving the state it was last seen in.
//
// paho, with auto-reconnect and connect-retry enabled, reports IsConnected true
// while it is only *trying* to reconnect. Trusting that leaves a powered-off
// printer looking connected forever, and monitorBambu then republishes its last
// cached report - an A1 that was idle when unplugged stays "idle" on the
// dashboard indefinitely.
func TestBambuPoweredOffPrinterReadsOffline(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	b := newTestBridge(t, printer, spoolman)
	config, stub := bambuTestPrinter(t, b, bambuStateIdle, "")

	if err := b.monitorBambu("printer_bambu", config); err != nil {
		t.Fatalf("monitorBambu: %v", err)
	}
	if cached, fresh := b.cachedPrinterState("printer_bambu"); !fresh || cached.state != StateIdle {
		t.Fatalf("connected printer should read idle, got %q (fresh=%v)", cached.state, fresh)
	}

	// Printer unplugged. paho still calls itself connected while retrying.
	stub.powerOff()
	if !stub.IsConnected() {
		t.Fatal("test premise wrong: paho reports connected while reconnecting")
	}

	if err := b.monitorBambu("printer_bambu", config); err != nil {
		t.Fatalf("monitorBambu after power off: %v", err)
	}
	cached, fresh := b.cachedPrinterState("printer_bambu")
	if !fresh || cached.state != StateOffline {
		t.Errorf("powered-off printer state = %q (fresh=%v), want %q", cached.state, fresh, StateOffline)
	}

	// And the dashboard agrees, rather than serving the pre-drop report.
	status, err := b.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	if got := status.Printers["printer_bambu"].State; got != StateOffline {
		t.Errorf("dashboard state for powered-off printer = %q, want %q", got, StateOffline)
	}
}

// TestBambuUnreportedPrinterReadsOffline covers the case that bites on startup
// with the printer switched off: paho's retry loop leaves the client claiming to
// be connected, no report ever arrives, and the state is simply unknown. Idle
// would be a guess that reads as a healthy, unused printer - offline is the
// honest answer for a printer that has told us nothing.
func TestBambuUnreportedPrinterReadsOffline(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	b := newTestBridge(t, printer, spoolman)
	config, _ := bambuTestPrinter(t, b, bambuStateIdle, "")

	// Live session, but the printer has not reported yet.
	b.existingBambuClient("printer_bambu").invalidateReport()

	if err := b.monitorBambu("printer_bambu", config); err != nil {
		t.Fatalf("monitorBambu: %v", err)
	}
	cached, fresh := b.cachedPrinterState("printer_bambu")
	if !fresh || cached.state != StateOffline {
		t.Errorf("state with no report = %q (fresh=%v), want %q", cached.state, fresh, StateOffline)
	}
}

// TestBambuReportInvalidatedOnConnectionLoss: once the session drops, the
// cached report is no longer evidence of anything, so a reconnect serves fresh
// state rather than what the printer was doing before it vanished.
func TestBambuReportInvalidatedOnConnectionLoss(t *testing.T) {
	bc, _ := stubBambuClient("10.0.0.9", "SERIAL1", "code", bambuStateRunning, "part.3mf")
	if _, ok := bc.snapshot(); !ok {
		t.Fatal("setup should have left a report cached")
	}

	bc.invalidateReport()

	if _, ok := bc.snapshot(); ok {
		t.Error("report survived the connection loss")
	}
}

// TestRetireStaleBambuClients: deleting a Bambu printer must close its MQTT
// session instead of leaving it open for the life of the process.
func TestRetireStaleBambuClients(t *testing.T) {
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	b := newTestBridge(t, printer, spoolman)
	bambuTestPrinter(t, b, bambuStateRunning, "part.3mf")

	bc := b.existingBambuClient("printer_bambu")
	if bc == nil {
		t.Fatal("test setup did not register a Bambu client")
	}

	// Config no longer lists the printer.
	b.retireStaleBambuClients(map[string]PrinterConfig{})

	if b.existingBambuClient("printer_bambu") != nil {
		t.Error("client for a deleted printer was kept")
	}
	if bc.isConnected() {
		t.Error("client for a deleted printer was left connected")
	}
}

func TestBambuJobName(t *testing.T) {
	if n := bambuJobName(bambuReport{}); n != "No active job" {
		t.Errorf("empty report name = %q", n)
	}
	var r bambuReport
	r.Print.GcodeFile = "file.3mf"
	if n := bambuJobName(r); n != "file.3mf" {
		t.Errorf("filename fallback = %q", n)
	}
	r.Print.SubtaskName = "My Print"
	if n := bambuJobName(r); n != "My Print" {
		t.Errorf("subtask-name preference = %q", n)
	}
}
