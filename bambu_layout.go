package main

// What a Bambu printer says it is made of, and how its filament sources map onto
// filament positions.
//
// Everything here is parsed leniently and defensively. Field types vary across
// models and firmware (tray ids arrive as strings, an A1 has no AMS block at
// all), and a report that does not parse must leave the last known layout
// standing rather than emptying it. All knowledge of Bambu's wire encoding lives
// in decodeBambuSource, so the guesswork is in one place with fixtures pinned to
// real captures.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Sources the printer reports for a filament. The first two are the ids of the
// external spool holders as the printer numbers them; the rest are AMS trays.
const (
	bambuSlotUnused   = 65535 // a project filament slot this print does not use
	bambuExternalMain = 255   // vir_slot 255, and vt_tray on single nozzle printers
	bambuExternalAlt  = 254   // the second holder on a dual nozzle machine

	// bambuExternalSpoolValue is how the main external holder appears in a
	// mapping, being slot 255 in the unit<<8|slot form (0xFF00).
	bambuExternalSpoolValue = bambuExternalMain << 8
)

// lenientJSONInt accepts the number or the quoted number Bambu uses
// interchangeably for the same field across models and firmware.
type lenientJSONInt int

func (n *lenientJSONInt) UnmarshalJSON(raw []byte) error {
	*n = lenientJSONInt(lenientInt(raw))
	return nil
}

// bambuTray is one AMS slot and what is loaded in it.
type bambuTray struct {
	ID       int
	Material string // "PLA", "PETG"; empty when the slot holds nothing
	Color    string // RRGGBBAA as the printer reports it
}

// bambuAMSUnit is one AMS attached to the printer.
type bambuAMSUnit struct {
	ID    int
	Trays []bambuTray
}

// bambuExternalSlot is an external spool holder.
type bambuExternalSlot struct {
	ID       int
	Material string
	Color    string
}

// bambuLayout is the printer's physical filament layout.
type bambuLayout struct {
	Units     []bambuAMSUnit
	Externals []bambuExternalSlot
	Nozzles   int
}

// Empty reports whether the printer described no filament sources at all, which
// is what a report that carries no filament blocks looks like.
func (l bambuLayout) Empty() bool {
	return len(l.Units) == 0 && len(l.Externals) == 0
}

// parseBambuLayout reads a report's filament blocks. The second result says
// whether this was a complete state push, which is the only kind that can be
// trusted to say a source is *gone*: the small delta reports an A1 sends carry
// changed scalars only, and would otherwise read as a printer with no AMS.
//
// Captured evidence: a full push is `command: "push_status", msg: 0` and carries
// the ams and vir_slot/vt_tray blocks. Deltas are msg 1 and carry neither. An
// X2D sends only full pushes, every few seconds.
func parseBambuLayout(payload []byte) (bambuLayout, bool) {
	var raw struct {
		Print struct {
			Command string `json:"command"`
			Msg     *int   `json:"msg"`
			AMS     struct {
				AMS []struct {
					ID   lenientJSONInt `json:"id"`
					Tray []struct {
						ID    lenientJSONInt `json:"id"`
						Type  string         `json:"tray_type"`
						Color string         `json:"tray_color"`
					} `json:"tray"`
				} `json:"ams"`
			} `json:"ams"`
			VirSlot []struct {
				ID    lenientJSONInt `json:"id"`
				Type  string         `json:"tray_type"`
				Color string         `json:"tray_color"`
			} `json:"vir_slot"`
			VTTray struct {
				ID    *lenientJSONInt `json:"id"`
				Type  string          `json:"tray_type"`
				Color string          `json:"tray_color"`
			} `json:"vt_tray"`
			Device struct {
				Nozzle struct {
					Info []struct {
						ID lenientJSONInt `json:"id"`
					} `json:"info"`
				} `json:"nozzle"`
			} `json:"device"`
		} `json:"print"`
	}
	if json.Unmarshal(payload, &raw) != nil {
		return bambuLayout{}, false
	}

	var layout bambuLayout
	for _, u := range raw.Print.AMS.AMS {
		unit := bambuAMSUnit{ID: int(u.ID)}
		for _, t := range u.Tray {
			unit.Trays = append(unit.Trays, bambuTray{ID: int(t.ID), Material: t.Type, Color: t.Color})
		}
		layout.Units = append(layout.Units, unit)
	}
	for _, v := range raw.Print.VirSlot {
		layout.Externals = append(layout.Externals, bambuExternalSlot{ID: int(v.ID), Material: v.Type, Color: v.Color})
	}
	// Single nozzle printers report one external holder as vt_tray instead.
	if raw.Print.VTTray.ID != nil {
		layout.Externals = append(layout.Externals, bambuExternalSlot{
			ID: int(*raw.Print.VTTray.ID), Material: raw.Print.VTTray.Type, Color: raw.Print.VTTray.Color,
		})
	}
	layout.Nozzles = len(raw.Print.Device.Nozzle.Info)
	if layout.Nozzles == 0 {
		layout.Nozzles = 1 // no device block means a single nozzle machine
	}

	// A report with no filament blocks describes nothing, whatever its msg says.
	complete := raw.Print.Command == "push_status" && raw.Print.Msg != nil && *raw.Print.Msg == 0 && !layout.Empty()
	return layout, complete
}

// decodeBambuSource turns one entry of the printer's per-filament mapping into a
// position key. This is the only place that knows Bambu's encoding.
//
// Values seen on real printers:
//   - 65535 marks a project filament slot the print does not use. Not an error.
//   - 0xFF00 and 0xFE00 are the external holders, being slot 255 and 254 in the
//     unit<<8|slot form an X2D uses.
//   - 254 bare is the external holder on single nozzle printers (vt_tray).
//   - 0..3 are AMS 0's trays, which both encodings agree on.
//   - 4..253 are a flat tray number across units, as older multi-AMS printers
//     report. Unverified here: no such capture exists yet.
//
// A value the layout has no source for is refused rather than guessed at.
func decodeBambuSource(value int, layout bambuLayout) (string, bool) {
	key, ok := bambuSourceKey(value)
	if !ok || !layoutHasSource(layout, key) {
		return "", false
	}
	return key, true
}

// bambuSourceKey is the encoding itself, split out so it can be tested without a
// layout to check against.
func bambuSourceKey(value int) (string, bool) {
	switch {
	case value == bambuSlotUnused || value < 0:
		return "", false
	case value >= 256:
		unit, slot := value>>8, value&0xFF
		if unit == bambuExternalMain || unit == bambuExternalAlt {
			return externalPositionKey(unit), true
		}
		return amsPositionKey(unit, slot), true
	case value == bambuExternalMain || value == bambuExternalAlt:
		return externalPositionKey(value), true
	case value < 4:
		return amsPositionKey(0, value), true
	default:
		// Flat numbering across units, four trays each.
		return amsPositionKey(value/4, value%4), true
	}
}

// layoutHasSource reports whether the printer actually has the place a key
// names, so a mapping value that decodes cleanly but points at hardware the
// printer never reported is still refused.
func layoutHasSource(layout bambuLayout, key string) bool {
	for _, u := range layout.Units {
		for _, t := range u.Trays {
			if amsPositionKey(u.ID, t.ID) == key {
				return true
			}
		}
	}
	for _, e := range layout.Externals {
		if externalPositionKey(e.ID) == key {
			return true
		}
	}
	return false
}

func amsPositionKey(unit, slot int) string { return fmt.Sprintf("ams:%d:%d", unit, slot) }
func externalPositionKey(slot int) string  { return fmt.Sprintf("ext:%d", slot) }

// bambuLayoutPositions lists every place this printer can hold filament, in the
// order they should be presented: AMS units by id and slot, then the external
// holders.
func bambuLayoutPositions(layout bambuLayout) []string {
	var keys []string
	for _, u := range layout.Units {
		for _, t := range u.Trays {
			keys = append(keys, amsPositionKey(u.ID, t.ID))
		}
	}
	for _, e := range layout.Externals {
		keys = append(keys, externalPositionKey(e.ID))
	}
	return keys
}

// bambuLayoutSignature is a comparable form of a layout, so a printer repeating
// the same state every few seconds does not look like a change. Only what
// decides positions is included: which places exist, and how many nozzles, which
// is what names the external holders.
func bambuLayoutSignature(layout bambuLayout) string {
	return fmt.Sprintf("%d|%s", layout.Nozzles, strings.Join(bambuLayoutPositions(layout), ","))
}

// bambuPositionLabel names a position the way the user sees it on the printer.
// Bambu labels AMS units A, B, C, D and counts slots from one, so this follows
// that rather than inventing its own numbering. Labels must never contain " - ",
// which separates the printer's name from the position in a Spoolman location.
//
// Which external holder feeds which nozzle is inferred from one capture (an X2D
// printing from vir_slot 255 on the right-hand nozzle), so it is only used for
// the label. The position's identity is the holder's own id.
func bambuPositionLabel(key string, layout bambuLayout) string {
	var unit, slot int
	if n, err := fmt.Sscanf(key, "ams:%d:%d", &unit, &slot); err == nil && n == 2 {
		if unit >= 128 {
			return fmt.Sprintf("AMS HT %d Slot %d", unit-127, slot+1)
		}
		return fmt.Sprintf("AMS %c Slot %d", 'A'+rune(unit), slot+1)
	}
	if n, err := fmt.Sscanf(key, "ext:%d", &slot); err == nil && n == 1 {
		if layout.Nozzles < 2 {
			return "External"
		}
		if slot == bambuExternalAlt {
			return "External Left"
		}
		return "External Right"
	}
	return key
}
