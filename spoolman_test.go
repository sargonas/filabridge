package main

import (
	"sort"
	"testing"
)

// namesOf extracts the Name field from each location, sorted, for
// order-independent comparison.
func namesOf(locs []SpoolmanLocation) []string {
	out := make([]string, 0, len(locs))
	for _, l := range locs {
		out = append(out, l.Name)
	}
	sort.Strings(out)
	return out
}

func TestGetLocations(t *testing.T) {
	cases := []struct {
		name      string
		locations []string // served at /api/v1/location
		setting   []string // nil => setting endpoint 404s
		want      []string
	}{
		{
			name:      "empty setting falls through to list",
			locations: []string{"Closet Shelf", "Printer A"},
			setting:   []string{}, // set but empty
			want:      []string{"Closet Shelf", "Printer A"},
		},
		{
			name:      "setting endpoint 404s, list still used",
			locations: []string{"Closet Shelf", "Printer A"},
			setting:   nil, // nil => 404 => setting source errors
			want:      []string{"Closet Shelf", "Printer A"},
		},
		{
			name:      "union and dedupe by exact name",
			locations: []string{"Closet Shelf", "Printer A"},
			setting:   []string{"Closet Shelf", "Bin 3"},
			want:      []string{"Bin 3", "Closet Shelf", "Printer A"}, // "Closet Shelf" once
		},
		{
			name:      "empty and whitespace names are skipped",
			locations: []string{"Printer A", "", "   "},
			setting:   []string{"Bin 3", ""},
			want:      []string{"Bin 3", "Printer A"},
		},
		{
			name:      "setting only (list empty)",
			locations: []string{},
			setting:   []string{"Bin 3", "Closet Shelf"},
			want:      []string{"Bin 3", "Closet Shelf"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeSpoolman(t)
			srv.Locations = tc.locations
			srv.LocationSetting = tc.setting

			client := NewSpoolmanClient(srv.URL(), 5, "", "")
			got, err := client.GetLocations()
			if err != nil {
				t.Fatalf("GetLocations returned error: %v", err)
			}

			gotNames := namesOf(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)

			if len(gotNames) != len(want) {
				t.Fatalf("got %v, want %v", gotNames, want)
			}
			for i := range gotNames {
				if gotNames[i] != want[i] {
					t.Fatalf("got %v, want %v", gotNames, want)
				}
			}
		})
	}
}
