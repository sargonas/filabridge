package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
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
	srv := newFakeSpoolman(t)
	srv.Locations = []string{"Closet Shelf", "", "Printer A"}

	client := NewSpoolmanClient(srv.URL(), 5, "", "")
	got, err := client.GetLocations()
	if err != nil {
		t.Fatalf("GetLocations returned error: %v", err)
	}

	want := []string{"Closet Shelf", "Printer A", "Unassigned"}
	gotNames := namesOf(got)
	sort.Strings(want)
	if !reflect.DeepEqual(gotNames, want) {
		t.Fatalf("got %v, want %v", gotNames, want)
	}
}

// TestGetLocationsFromList pins the response shapes we accept from
// GET /api/v1/location and the single normalization pass applied to all of
// them: blank names become "Unassigned" (mirroring Spoolman's own UI) and the
// result is deduped by final name.
func TestGetLocationsFromList(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "array of objects",
			body: `[{"id":1,"name":"Closet Shelf"},{"id":2,"name":"Printer A"}]`,
			want: []string{"Closet Shelf", "Printer A"},
		},
		{
			name: "data wrapper",
			body: `{"data":[{"id":1,"name":"Closet Shelf"}]}`,
			want: []string{"Closet Shelf"},
		},
		{
			name: "results wrapper",
			body: `{"results":[{"id":1,"name":"Printer A"}]}`,
			want: []string{"Printer A"},
		},
		{
			name: "plain array of names",
			body: `["Closet Shelf","Printer A"]`,
			want: []string{"Closet Shelf", "Printer A"},
		},
		{
			name: "blank name normalizes to Unassigned",
			body: `[{"id":1,"name":""},{"id":2,"name":"Printer A"}]`,
			want: []string{"Printer A", "Unassigned"},
		},
		{
			name: "whitespace-only name normalizes to Unassigned",
			body: `["   ","Printer A"]`,
			want: []string{"Printer A", "Unassigned"},
		},
		{
			name: "multiple blanks collapse to one Unassigned",
			body: `[{"id":1,"name":""},{"id":2,"name":"  "},{"id":3,"name":"Printer A"}]`,
			want: []string{"Printer A", "Unassigned"},
		},
		{
			name: "blank and literal Unassigned do not duplicate",
			body: `["","Unassigned","Printer A"]`,
			want: []string{"Printer A", "Unassigned"},
		},
		{
			name: "duplicate names deduped",
			body: `["Printer A","Printer A"]`,
			want: []string{"Printer A"},
		},
		{
			name: "empty list",
			body: `[]`,
			want: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			client := NewSpoolmanClient(srv.URL, 5, "", "")
			got, err := client.getLocationsFromList()
			if err != nil {
				t.Fatalf("getLocationsFromList error: %v", err)
			}
			gotNames := namesOf(got)
			want := make([]string, 0, len(tc.want))
			want = append(want, tc.want...)
			sort.Strings(want)
			if !reflect.DeepEqual(gotNames, want) {
				t.Fatalf("got %v, want %v", gotNames, want)
			}
		})
	}

	t.Run("unexpected shape errors", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"unexpected":"shape"}`))
		}))
		defer srv.Close()

		client := NewSpoolmanClient(srv.URL, 5, "", "")
		if _, err := client.getLocationsFromList(); err == nil {
			t.Fatal("expected an error for an unrecognized JSON shape, got nil")
		}
	})
}

func TestUpdateLocationByName(t *testing.T) {
	t.Run("renames all spools carrying the old name", func(t *testing.T) {
		srv := newFakeSpoolman(t)
		srv.Spools = map[int]*fakeSpool{
			1: {ID: 1, Location: "Closet Shelf"},
			2: {ID: 2, Location: "Closet Shelf"},
			3: {ID: 3, Location: "Printer A"},
		}

		client := NewSpoolmanClient(srv.URL(), 5, "", "")
		if err := client.UpdateLocationByName("Closet Shelf", "Garage"); err != nil {
			t.Fatalf("UpdateLocationByName error: %v", err)
		}

		if srv.Spools[1].Location != "Garage" || srv.Spools[2].Location != "Garage" {
			t.Fatalf("expected spools 1 and 2 to move to Garage, got %q and %q",
				srv.Spools[1].Location, srv.Spools[2].Location)
		}
		if srv.Spools[3].Location != "Printer A" {
			t.Fatalf("spool 3 should be untouched, got %q", srv.Spools[3].Location)
		}
		if len(srv.PatchCalls) != 2 {
			t.Fatalf("expected 2 spools patched, got %d (%v)", len(srv.PatchCalls), srv.PatchCalls)
		}
	})

	t.Run("renaming Unassigned targets empty-string locations", func(t *testing.T) {
		srv := newFakeSpoolman(t)
		srv.Spools = map[int]*fakeSpool{
			1: {ID: 1, Location: ""},
			2: {ID: 2, Location: "Printer A"},
		}

		client := NewSpoolmanClient(srv.URL(), 5, "", "")
		if err := client.UpdateLocationByName("Unassigned", "Shelf"); err != nil {
			t.Fatalf("UpdateLocationByName error: %v", err)
		}
		if srv.Spools[1].Location != "Shelf" {
			t.Fatalf("expected empty-location spool to move to Shelf, got %q", srv.Spools[1].Location)
		}
	})

	t.Run("renaming TO Unassigned clears the location", func(t *testing.T) {
		srv := newFakeSpoolman(t)
		srv.Spools = map[int]*fakeSpool{
			1: {ID: 1, Location: "Printer A"},
		}

		client := NewSpoolmanClient(srv.URL(), 5, "", "")
		if err := client.UpdateLocationByName("Printer A", "Unassigned"); err != nil {
			t.Fatalf("UpdateLocationByName error: %v", err)
		}
		if srv.Spools[1].Location != "" {
			t.Fatalf("expected location cleared to empty, got %q", srv.Spools[1].Location)
		}
	})

	t.Run("no matching spools returns error", func(t *testing.T) {
		srv := newFakeSpoolman(t)
		srv.Spools = map[int]*fakeSpool{
			1: {ID: 1, Location: "Printer A"},
		}

		client := NewSpoolmanClient(srv.URL(), 5, "", "")
		err := client.UpdateLocationByName("Nonexistent", "Whatever")
		if err == nil {
			t.Fatalf("expected error when no spools match, got nil")
		}
	})
}
