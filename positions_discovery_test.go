package main

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// x2dLayoutReport is a report shaped like a real X2D full push: one AMS of four
// trays, two external holders, two nozzles.
func x2dLayoutReport(trays int, units int) string {
	var amsUnits []string
	for u := 0; u < units; u++ {
		var traySpecs []string
		for t := 0; t < trays; t++ {
			traySpecs = append(traySpecs, fmt.Sprintf(`{"id":"%d","tray_type":"PLA"}`, t))
		}
		amsUnits = append(amsUnits, fmt.Sprintf(`{"id":"%d","tray":[%s]}`, u, strings.Join(traySpecs, ",")))
	}
	return fmt.Sprintf(`{"print":{"command":"push_status","msg":0,
		"ams":{"ams":[%s]},
		"vir_slot":[{"id":"254","tray_type":""},{"id":"255","tray_type":"PLA"}],
		"device":{"nozzle":{"info":[{"id":0},{"id":1}]}}}}`, strings.Join(amsUnits, ","))
}

// bambuDiscoveryBridge stands up a Bambu printer whose client can be fed reports.
func bambuDiscoveryBridge(t *testing.T) (*FilamentBridge, PrinterConfig, *bambuClient) {
	t.Helper()
	printer := newFakePrusaLink(t)
	spoolman := newFakeSpoolman(t)
	b := newTestBridge(t, printer, spoolman)
	config, _ := bambuTestPrinter(t, b, bambuStateIdle, "")
	return b, config, b.existingBambuClient("printer_bambu")
}

// TestDiscoveryAllocatesPositionsFromLayout: the printer's own layout decides
// what places it has, named the way its screen names them.
func TestDiscoveryAllocatesPositionsFromLayout(t *testing.T) {
	b, config, bc := bambuDiscoveryBridge(t)
	bc.onMessage(nil, fakeMQTTMessage{[]byte(x2dLayoutReport(4, 1))})

	if err := b.monitorBambu("printer_bambu", config); err != nil {
		t.Fatalf("monitorBambu: %v", err)
	}

	got := positionSummary(t, b, "printer_bambu")
	want := []string{
		"ams:0:0 id=0 label=AMS A Slot 1 present",
		"ams:0:1 id=1 label=AMS A Slot 2 present",
		"ams:0:2 id=2 label=AMS A Slot 3 present",
		"ams:0:3 id=3 label=AMS A Slot 4 present",
		"ext:254 id=4 label=External Left present",
		"ext:255 id=5 label=External Right present",
	}
	if len(got) != len(want) {
		t.Fatalf("positions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d = %q, want %q", i, got[i], want[i])
		}
	}

	// Repeating the same state changes nothing, which matters because an X2D
	// sends a full push every few seconds.
	for i := 0; i < 3; i++ {
		bc.onMessage(nil, fakeMQTTMessage{[]byte(x2dLayoutReport(4, 1))})
		if err := b.monitorBambu("printer_bambu", config); err != nil {
			t.Fatal(err)
		}
	}
	if again := positionSummary(t, b, "printer_bambu"); len(again) != len(want) {
		t.Errorf("repeated reports changed the positions: %v", again)
	}
}

// TestDiscoveryKeepsIdsWhenHardwareComesAndGoes: unplugging an AMS must not
// renumber anything or drop what its slots hold. Plugging it back in returns the
// same ids, so a spool mapped to AMS A slot 3 is still there.
func TestDiscoveryKeepsIdsWhenHardwareComesAndGoes(t *testing.T) {
	b, config, bc := bambuDiscoveryBridge(t)
	bc.onMessage(nil, fakeMQTTMessage{[]byte(x2dLayoutReport(4, 2))})
	if err := b.monitorBambu("printer_bambu", config); err != nil {
		t.Fatal(err)
	}

	before := map[string]int{}
	positions, err := b.listPositions("printer_bambu")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range positions {
		before[p.Key] = p.ID
	}
	if len(before) != 10 {
		t.Fatalf("two AMS units and two holders should be 10 positions, got %d", len(before))
	}

	// The second AMS is unplugged.
	bc.onMessage(nil, fakeMQTTMessage{[]byte(x2dLayoutReport(4, 1))})
	if err := b.monitorBambu("printer_bambu", config); err != nil {
		t.Fatal(err)
	}
	positions, err = b.listPositions("printer_bambu")
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 10 {
		t.Fatalf("unplugging an AMS deleted positions: %v", positionSummary(t, b, "printer_bambu"))
	}
	for _, p := range positions {
		wantPresent := !strings.HasPrefix(p.Key, "ams:1:")
		if p.Present != wantPresent {
			t.Errorf("%s present = %v, want %v", p.Key, p.Present, wantPresent)
		}
		if before[p.Key] != p.ID {
			t.Errorf("%s changed id from %d to %d", p.Key, before[p.Key], p.ID)
		}
	}

	// Plugged back in: same ids, present again.
	bc.onMessage(nil, fakeMQTTMessage{[]byte(x2dLayoutReport(4, 2))})
	if err := b.monitorBambu("printer_bambu", config); err != nil {
		t.Fatal(err)
	}
	positions, err = b.listPositions("printer_bambu")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range positions {
		if !p.Present {
			t.Errorf("%s stayed absent after the AMS came back", p.Key)
		}
		if before[p.Key] != p.ID {
			t.Errorf("%s changed id from %d to %d", p.Key, before[p.Key], p.ID)
		}
	}
}

// TestDiscoveryAppendsNewHardware: an AMS added later takes new ids and leaves
// everything already allocated alone, but still sorts among the AMS slots rather
// than after the external holders it was allocated behind.
func TestDiscoveryAppendsNewHardware(t *testing.T) {
	b, config, bc := bambuDiscoveryBridge(t)
	bc.onMessage(nil, fakeMQTTMessage{[]byte(x2dLayoutReport(4, 1))})
	if err := b.monitorBambu("printer_bambu", config); err != nil {
		t.Fatal(err)
	}
	bc.onMessage(nil, fakeMQTTMessage{[]byte(x2dLayoutReport(4, 2))})
	if err := b.monitorBambu("printer_bambu", config); err != nil {
		t.Fatal(err)
	}

	positions, err := b.listPositions("printer_bambu")
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	byKey := map[string]int{}
	for _, p := range positions {
		order = append(order, p.Key)
		byKey[p.Key] = p.ID
	}
	// The externals kept ids 4 and 5; the new unit's slots took 6 onward.
	if byKey["ext:254"] != 4 || byKey["ext:255"] != 5 || byKey["ams:1:0"] != 6 {
		t.Errorf("ids were reshuffled: %v", byKey)
	}
	want := "ams:0:0,ams:0:1,ams:0:2,ams:0:3,ams:1:0,ams:1:1,ams:1:2,ams:1:3,ext:254,ext:255"
	if strings.Join(order, ",") != want {
		t.Errorf("display order = %v, want the new unit among the AMS slots", order)
	}
}

// TestDiscoveryIgnoresDeltasAndDisconnects: only a complete push says what a
// printer has. A delta, or losing the connection, must leave the layout alone
// rather than reading as a printer that lost its AMS.
func TestDiscoveryIgnoresDeltasAndDisconnects(t *testing.T) {
	b, config, bc := bambuDiscoveryBridge(t)
	bc.onMessage(nil, fakeMQTTMessage{[]byte(x2dLayoutReport(4, 1))})
	if err := b.monitorBambu("printer_bambu", config); err != nil {
		t.Fatal(err)
	}
	before := positionSummary(t, b, "printer_bambu")

	bc.onMessage(nil, fakeMQTTMessage{[]byte(`{"print":{"command":"push_status","msg":1,"bed_temper":41.5}}`)})
	bc.invalidateReport()
	bc.onMessage(nil, fakeMQTTMessage{[]byte(x2dLayoutReport(4, 1))})
	if err := b.monitorBambu("printer_bambu", config); err != nil {
		t.Fatal(err)
	}

	after := positionSummary(t, b, "printer_bambu")
	if strings.Join(before, "|") != strings.Join(after, "|") {
		t.Errorf("layout changed across a delta and a reconnect:\nbefore %v\nafter  %v", before, after)
	}
}

// TestDiscoveryIsSafeConcurrently: the monitor loop and anything else touching
// positions must not race or double-insert.
func TestDiscoveryIsSafeConcurrently(t *testing.T) {
	b, config, bc := bambuDiscoveryBridge(t)
	bc.onMessage(nil, fakeMQTTMessage{[]byte(x2dLayoutReport(4, 1))})

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.monitorBambu("printer_bambu", config); err != nil {
				t.Errorf("monitorBambu: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := positionSummary(t, b, "printer_bambu"); len(got) != 6 {
		t.Errorf("concurrent discovery produced %d positions, want 6: %v", len(got), got)
	}
}

// TestBambuLegacyPositionsReserved: a database from before the printer's layout
// was read has numbered toolheads standing in for AMS slots. They are kept so
// their mappings stay visible and undoable, but marked absent and re-keyed, and
// discovery allocates the real positions alongside them without reusing ids that
// history already points at.
func TestBambuLegacyPositionsReserved(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FILABRIDGE_DB_PATH", dir)

	db, err := sql.Open("sqlite", dir+"/"+DefaultDBFileName)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE printer_configs (printer_id TEXT PRIMARY KEY, name TEXT NOT NULL, ip_address TEXT NOT NULL, api_key TEXT, toolheads INTEGER DEFAULT 1, type TEXT NOT NULL DEFAULT 'prusalink', serial TEXT NOT NULL DEFAULT '', created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO printer_configs (printer_id, name, ip_address, api_key, toolheads, type, serial) VALUES ('pb', 'X2D', '10.0.0.9', 'code', 5, 'bambu', 'SERIAL');
		INSERT INTO printer_configs (printer_id, name, ip_address, api_key, toolheads, type) VALUES ('pp', 'XL', '10.0.0.5', 'k', 3, 'prusalink');
		CREATE TABLE toolhead_mappings (printer_id TEXT, toolhead_id INTEGER, spool_id INTEGER, mapped_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, PRIMARY KEY (printer_id, toolhead_id));
		INSERT INTO toolhead_mappings (printer_id, toolhead_id, spool_id) VALUES ('pb', 0, 18);
		INSERT INTO toolhead_mappings (printer_id, toolhead_id, spool_id) VALUES ('pb', 4, 15);
	`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	bridge, err := NewFilamentBridge(nil)
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	defer bridge.Close()

	// The Bambu printer's old toolheads are reserved, not deleted, so the spools
	// they hold can still be seen and freed.
	got := positionSummary(t, bridge, "pb")
	want := []string{
		"legacy:0 id=0 label=Toolhead 0 absent",
		"legacy:4 id=4 label=Toolhead 4 absent",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("reserved positions = %v, want %v", got, want)
	}
	if id, err := bridge.GetToolheadMapping("X2D", 4); err != nil || id != 15 {
		t.Errorf("reserved mapping = %d (%v), want spool 15 still visible", id, err)
	}

	// The Prusa printer is untouched by any of this.
	if prusa := positionSummary(t, bridge, "pp"); len(prusa) != 3 || !strings.HasPrefix(prusa[0], "toolhead:0 ") {
		t.Errorf("Prusa positions disturbed: %v", prusa)
	}

	// Discovery then allocates the real places without reusing ids 0 or 4.
	if err := bridge.migrateTx("test discovery", func(tx *sql.Tx) error {
		layout, _ := parseBambuLayout([]byte(x2dLayoutReport(4, 1)))
		return reconcileDiscoveredPositions(tx, "pb", layout)
	}); err != nil {
		t.Fatal(err)
	}
	positions, err := bridge.listPositions("pb")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range positions {
		if strings.HasPrefix(p.Key, positionKeyLegacy+":") {
			continue
		}
		if p.ID == 0 || p.ID == 4 {
			t.Errorf("discovery reused id %d, which history and mappings point at: %+v", p.ID, p)
		}
	}
	if len(positions) != 8 {
		t.Errorf("want 2 reserved plus 6 discovered, got %v", positionSummary(t, bridge, "pb"))
	}
}
