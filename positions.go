package main

// Filament positions: the places a spool can be loaded on a printer.
//
// A printer used to be a count of "toolheads", numbered 0..N-1, which describes
// a Prusa but not a Bambu machine, where the places are AMS slots and external
// spool holders across one or two nozzles. A position is instead identified by
// three things, each with one job:
//
//	id     an integer, allocated once and never reused. Everything else in the
//	       database references this: mappings, history, warnings, the webhook.
//	key    what the position is (toolhead:1, ams:0:2, ext:255). Used to match
//	       hardware when a printer reports its own layout, and rewritten by
//	       explicit migrations as more is learned about what printers report.
//	label  what the user and Spoolman call it ("Toolhead 1"). It ends up in
//	       Spoolman location strings and on printed NFC tags, so it is frozen
//	       when the position is created and only a rename changes it.
//
// For a PrusaLink printer the layout is just its toolhead count, so position id
// N is always key toolhead:N labelled "Toolhead N", which is byte-identical to
// what the count-based code produced.

import (
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
)

// positionKeyToolhead names a plain toolhead, the only kind a PrusaLink printer
// has. Bambu keys (ams:unit:slot, ext:slot) arrive with layout discovery.
const positionKeyToolhead = "toolhead"

// positionKeyLegacy marks a Bambu position from before the printer's own layout
// was read: a numbered toolhead that stood for "whatever filament 1 was". It
// cannot be matched to real hardware, so it is kept, absent, only so the
// mappings and history pointing at it stay visible and can be undone.
const positionKeyLegacy = "legacy"

// schemaVersionBambuLayout is the user_version stamped once Bambu positions have
// been reserved, so the one-way reservation never runs twice.
const schemaVersionBambuLayout = 1

// filamentPosition is one place a spool can be loaded.
type filamentPosition struct {
	ID      int    `json:"id"`
	Key     string `json:"key"`
	Label   string `json:"label"`
	Present bool   `json:"present"` // the printer still has it: an unplugged AMS leaves its mappings intact
}

// toolheadPositionKey is the key for a plain numbered toolhead.
func toolheadPositionKey(toolheadID int) string {
	return fmt.Sprintf("%s:%d", positionKeyToolhead, toolheadID)
}

// defaultToolheadLabel is what a numbered toolhead is called when the user has
// not renamed it. Unchanged from the count-based code, because this string is
// already baked into Spoolman locations and printed tags.
func defaultToolheadLabel(toolheadID int) string {
	return fmt.Sprintf("Toolhead %d", toolheadID)
}

// positionSortKey orders positions for display: numbered toolheads first, then
// AMS slots by unit and slot, then external holders. Ordering is derived from
// the key rather than the id, so a second AMS added later still sorts among the
// AMS slots instead of after the externals it was allocated behind.
func positionSortKey(p filamentPosition) (int, int, int) {
	parts := strings.Split(p.Key, ":")
	num := func(i int) int {
		if i >= len(parts) {
			return 0
		}
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return 0
		}
		return n
	}
	switch parts[0] {
	case positionKeyToolhead:
		return 0, num(1), 0
	case "ams":
		return 1, num(1), num(2)
	case "ext":
		return 2, num(1), 0
	default:
		return 3, p.ID, 0
	}
}

// listPositions returns a printer's positions in display order.
func (b *FilamentBridge) listPositions(printerID string) ([]filamentPosition, error) {
	rows, err := b.db.Query(
		`SELECT position_id, position_key, label, present FROM printer_positions WHERE printer_id = ?`,
		printerID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list positions: %w", err)
	}
	defer rows.Close()

	var positions []filamentPosition
	for rows.Next() {
		var p filamentPosition
		var present int
		if err := rows.Scan(&p.ID, &p.Key, &p.Label, &present); err != nil {
			return nil, err
		}
		p.Present = present != 0
		positions = append(positions, p)
	}
	sort.Slice(positions, func(i, j int) bool {
		ai, aj, ak := positionSortKey(positions[i])
		bi, bj, bk := positionSortKey(positions[j])
		if ai != bi {
			return ai < bi
		}
		if aj != bj {
			return aj < bj
		}
		if ak != bk {
			return ak < bk
		}
		return positions[i].ID < positions[j].ID
	})
	return positions, nil
}

// position returns one position of a printer by id.
func (b *FilamentBridge) position(printerID string, positionID int) (filamentPosition, bool) {
	positions, err := b.listPositions(printerID)
	if err != nil {
		return filamentPosition{}, false
	}
	for _, p := range positions {
		if p.ID == positionID {
			return p, true
		}
	}
	return filamentPosition{}, false
}

// reconcileToolheadPositions makes a printer's positions match its toolhead
// count: ids 0..count-1 exist and are present, and anything beyond the count is
// kept but marked absent, since mappings and history still reference it and its
// label may still be written on a tag. Positions are never renumbered or
// deleted, so an id always means the same place.
//
// Also creates an absent position for any id a mapping or a custom name still
// references, so nothing in the database points at a position that does not
// exist.
func reconcileToolheadPositions(tx *sql.Tx, printerID string, count int) error {
	known := map[int]bool{}
	rows, err := tx.Query(`SELECT position_id FROM printer_positions WHERE printer_id = ?`, printerID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		known[id] = true
	}
	rows.Close()

	// Ids referenced elsewhere must exist, even when they sit beyond the count.
	referenced := map[int]bool{}
	for _, q := range []string{
		`SELECT DISTINCT toolhead_id FROM toolhead_mappings WHERE printer_id = ?`,
		`SELECT DISTINCT toolhead_id FROM toolhead_names WHERE printer_id = ?`,
	} {
		r, err := tx.Query(q, printerID)
		if err != nil {
			return err
		}
		for r.Next() {
			var id int
			if err := r.Scan(&id); err != nil {
				r.Close()
				return err
			}
			referenced[id] = true
		}
		r.Close()
	}

	wanted := map[int]bool{}
	for id := 0; id < count; id++ {
		wanted[id] = true
	}

	for id := range union(wanted, referenced, known) {
		present := 0
		if wanted[id] {
			present = 1
		}
		if known[id] {
			if _, err := tx.Exec(
				`UPDATE printer_positions SET present = ? WHERE printer_id = ? AND position_id = ?`,
				present, printerID, id,
			); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO printer_positions (printer_id, position_id, position_key, label, present)
			 VALUES (?, ?, ?, ?, ?)`,
			printerID, id, toolheadPositionKey(id), defaultToolheadLabel(id), present,
		); err != nil {
			return err
		}
	}
	return nil
}

// reconcileDiscoveredPositions makes a printer's positions match the layout it
// reported. Places it has now are present; places it had before are kept and
// marked absent, because their mappings, history and printed tags still refer to
// them, and an AMS that is unplugged today may be back tomorrow. New places take
// the next free id, so an id never changes meaning.
//
// Positions reserved from before discovery (legacy:N) are left alone: they are
// already absent and only exist so the mappings they hold can be seen and undone.
func reconcileDiscoveredPositions(tx *sql.Tx, printerID string, layout bambuLayout) error {
	type existing struct {
		id  int
		key string
	}
	var rowsByKey = map[string]existing{}
	maxID := -1
	rows, err := tx.Query(`SELECT position_id, position_key FROM printer_positions WHERE printer_id = ?`, printerID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var e existing
		if err := rows.Scan(&e.id, &e.key); err != nil {
			rows.Close()
			return err
		}
		rowsByKey[e.key] = e
		if e.id > maxID {
			maxID = e.id
		}
	}
	rows.Close()

	wanted := map[string]bool{}
	for _, key := range bambuLayoutPositions(layout) {
		wanted[key] = true
		label := bambuPositionLabel(key, layout)
		if e, ok := rowsByKey[key]; ok {
			// The label is frozen at creation: it is already on tags and on
			// spools in Spoolman, so only presence is updated here.
			if _, err := tx.Exec(
				`UPDATE printer_positions SET present = 1 WHERE printer_id = ? AND position_id = ?`,
				printerID, e.id,
			); err != nil {
				return err
			}
			continue
		}
		maxID++
		if _, err := tx.Exec(
			`INSERT INTO printer_positions (printer_id, position_id, position_key, label, present)
			 VALUES (?, ?, ?, ?, 1)`,
			printerID, maxID, key, label,
		); err != nil {
			return err
		}
	}

	// Anything the printer no longer reports goes absent, except the reserved
	// rows, which are absent by definition.
	for key, e := range rowsByKey {
		if wanted[key] || strings.HasPrefix(key, positionKeyLegacy+":") {
			continue
		}
		if _, err := tx.Exec(
			`UPDATE printer_positions SET present = 0 WHERE printer_id = ? AND position_id = ?`,
			printerID, e.id,
		); err != nil {
			return err
		}
	}
	return nil
}

// union collects the ids present in any of the given sets.
func union(sets ...map[int]bool) map[int]bool {
	out := map[int]bool{}
	for _, set := range sets {
		for id := range set {
			out[id] = true
		}
	}
	return out
}

// syncBambuPositions gives a printer the positions its own layout describes,
// when that layout has changed since the last time. Writes happen under
// positionsMu, which is never held while waiting on anything else, and only for
// a printer that still exists: one deleted mid-cycle must not be recreated by
// its own last report.
func (b *FilamentBridge) syncBambuPositions(printerID string, client *bambuClient) {
	layout, seq := client.layoutSnapshot()
	if seq == 0 || layout.Empty() {
		return // the printer has not said what it has yet
	}

	b.positionsMu.Lock()
	defer b.positionsMu.Unlock()
	if b.bambuLayoutSeen[printerID] == seq {
		return
	}

	err := b.migrateTx("discover filament positions", func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM printer_configs WHERE printer_id = ?`, printerID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return nil
		}
		return reconcileDiscoveredPositions(tx, printerID, layout)
	})
	if err != nil {
		log.Printf("Warning: could not record filament positions for %s: %v", printerID, err)
		return
	}
	b.bambuLayoutSeen[printerID] = seq

	if positions, err := b.listPositions(printerID); err == nil {
		var names []string
		for _, p := range positions {
			if p.Present {
				names = append(names, p.Label)
			}
		}
		log.Printf("Filament positions on %s: %s", printerID, strings.Join(names, ", "))
	}
}

// reserveBambuToolheadPositions runs once, when a database first meets a build
// that reads a Bambu printer's own layout. Until now a Bambu printer's positions
// were numbered toolheads standing in for AMS slots, a mapping the user had to
// guess at, and nothing ties those numbers to the places the printer actually
// reports. They are marked absent and re-keyed legacy:N, which keeps their
// mappings visible and undoable while discovery allocates the real positions
// alongside them. Ids are never reused, so history keeps its meaning.
func (b *FilamentBridge) reserveBambuToolheadPositions() error {
	var version int
	if err := b.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version >= schemaVersionBambuLayout {
		return nil
	}

	configs, err := b.GetAllPrinterConfigs()
	if err != nil {
		return err
	}
	err = b.migrateTx("reserve Bambu toolhead positions", func(tx *sql.Tx) error {
		for printerID, cfg := range configs {
			if cfg.Type != PrinterTypeBambu {
				continue
			}
			if _, err := tx.Exec(
				`UPDATE printer_positions
				 SET position_key = ? || ':' || position_id, present = 0
				 WHERE printer_id = ? AND position_key LIKE ?`,
				positionKeyLegacy, printerID, positionKeyToolhead+":%",
			); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if _, err := b.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersionBambuLayout)); err != nil {
		return fmt.Errorf("stamp schema version: %w", err)
	}
	return nil
}

// reconcileAllPositions brings every configured printer's positions up to date.
// It is the migration as well as the maintenance path: a database that has never
// had positions gets them here, and running it twice changes nothing.
func (b *FilamentBridge) reconcileAllPositions() error {
	configs, err := b.GetAllPrinterConfigs()
	if err != nil {
		return err
	}
	return b.migrateTx("reconcile filament positions", func(tx *sql.Tx) error {
		for printerID, cfg := range configs {
			if cfg.Type == PrinterTypeBambu {
				// A Bambu printer's positions come from what it reports, not from
				// a count. Until it has reported, only the places something still
				// references exist, so nothing in the database dangles.
				if err := reserveReferencedPositions(tx, printerID); err != nil {
					return fmt.Errorf("printer %s: %w", printerID, err)
				}
				continue
			}
			if err := reconcileToolheadPositions(tx, printerID, cfg.Toolheads); err != nil {
				return fmt.Errorf("printer %s: %w", printerID, err)
			}
		}
		return nil
	})
}

// reserveReferencedPositions gives a printer an absent position for every id a
// mapping or custom name still points at, so nothing references a position that
// does not exist. Used for Bambu printers, whose real positions arrive with the
// printer's own layout.
func reserveReferencedPositions(tx *sql.Tx, printerID string) error {
	known := map[int]bool{}
	rows, err := tx.Query(`SELECT position_id FROM printer_positions WHERE printer_id = ?`, printerID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		known[id] = true
	}
	rows.Close()

	for _, q := range []string{
		`SELECT DISTINCT toolhead_id FROM toolhead_mappings WHERE printer_id = ?`,
		`SELECT DISTINCT toolhead_id FROM toolhead_names WHERE printer_id = ?`,
	} {
		r, err := tx.Query(q, printerID)
		if err != nil {
			return err
		}
		var ids []int
		for r.Next() {
			var id int
			if err := r.Scan(&id); err != nil {
				r.Close()
				return err
			}
			ids = append(ids, id)
		}
		r.Close()
		for _, id := range ids {
			if known[id] {
				continue
			}
			known[id] = true
			if _, err := tx.Exec(
				`INSERT INTO printer_positions (printer_id, position_id, position_key, label, present)
				 VALUES (?, ?, ?, ?, 0)`,
				printerID, id, fmt.Sprintf("%s:%d", positionKeyLegacy, id), defaultToolheadLabel(id),
			); err != nil {
				return err
			}
		}
	}
	return nil
}
