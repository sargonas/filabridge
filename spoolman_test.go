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

// TestGetLocations pins the fix for #44: locations come from /api/v1/location.
// The `locations` setting is not consulted, so a Spoolman v0.26 instance — which
// answers that setting with an empty, never-set value and HTTP 200 — still
// reports the locations its spools actually carry.
func TestGetLocations(t *testing.T) {
	t.Run("reads the location list", func(t *testing.T) {
		srv := newFakeSpoolman(t)
		srv.Locations = []string{"Closet Shelf", "Printer A"}

		client := NewSpoolmanClient(srv.URL(), 5, "", "")
		got, err := client.GetLocations()
		if err != nil {
			t.Fatalf("GetLocations returned error: %v", err)
		}

		want := []string{"Closet Shelf", "Printer A"}
		if gotNames := namesOf(got); !reflect.DeepEqual(gotNames, want) {
			t.Fatalf("got %v, want %v", gotNames, want)
		}
	})

	// The regression itself: before the fix, an empty-but-successful
	// setting response short-circuited GetLocations and returned nothing.
	t.Run("ignores an empty locations setting", func(t *testing.T) {
		var settingHits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/setting/locations":
				settingHits++
				_, _ = w.Write([]byte(`{"value":"[]","is_set":false}`))
			case "/api/v1/location":
				_, _ = w.Write([]byte(`["Closet Shelf","Printer A"]`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		client := NewSpoolmanClient(srv.URL, 5, "", "")
		got, err := client.GetLocations()
		if err != nil {
			t.Fatalf("GetLocations returned error: %v", err)
		}

		want := []string{"Closet Shelf", "Printer A"}
		if gotNames := namesOf(got); !reflect.DeepEqual(gotNames, want) {
			t.Fatalf("got %v, want %v — an empty setting must not suppress the list", gotNames, want)
		}
		if settingHits != 0 {
			t.Errorf("locations setting was requested %d times; it should not be consulted", settingHits)
		}
	})
}

// TestGetLocationsFromList pins the response shapes we accept from
// GET /api/v1/location, and that blank names are skipped rather than surfaced
// as nameless entries.
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
			name: "blank name skipped",
			body: `[{"id":1,"name":""},{"id":2,"name":"Printer A"}]`,
			want: []string{"Printer A"},
		},
		{
			name: "whitespace-only name skipped",
			body: `["   ","Printer A"]`,
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
