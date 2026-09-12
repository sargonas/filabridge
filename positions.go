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
	"sort"
	"strconv"
	"strings"
)

// positionKeyToolhead names a plain toolhead, the only kind a PrusaLink printer
// has. Bambu keys (ams:unit:slot, ext:slot) arrive with layout discovery.
const positionKeyToolhead = "toolhead"

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
			if err := reconcileToolheadPositions(tx, printerID, cfg.Toolheads); err != nil {
				return fmt.Errorf("printer %s: %w", printerID, err)
			}
		}
		return nil
	})
}
