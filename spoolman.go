package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// SpoolmanClient handles communication with Spoolman API for bridge functionality
type SpoolmanClient struct {
	baseURL    string
	httpClient *http.Client
	username   string
	password   string
}

// GetBaseURL returns the Spoolman base URL
func (c *SpoolmanClient) GetBaseURL() string {
	return c.baseURL
}

// SpoolmanSpool represents a spool from Spoolman API
type SpoolmanSpool struct {
	ID              int                    `json:"id"`
	Registered      string                 `json:"registered"`
	Filament        *SpoolmanFilament      `json:"filament"`
	RemainingWeight float64                `json:"remaining_weight"`
	InitialWeight   float64                `json:"initial_weight"`
	SpoolWeight     float64                `json:"spool_weight"`
	UsedWeight      float64                `json:"used_weight"`
	RemainingLength float64                `json:"remaining_length"`
	UsedLength      float64                `json:"used_length"`
	FirstUsed       string                 `json:"first_used"`
	LastUsed        string                 `json:"last_used"`
	Archived        bool                   `json:"archived"`
	LocationID      *int                   `json:"location_id"` // Reference to Spoolman Location entity
	Extra           map[string]interface{} `json:"extra"`

	// Computed fields for easier access
	Name     string `json:"name"`     // Computed from filament.name
	Brand    string `json:"brand"`    // Computed from filament.vendor.name
	Material string `json:"material"` // Computed from filament.material
	Location string `json:"location"` // Spool location (e.g., "Printer1 - Toolhead 0") - kept for backward compatibility
}

// SpoolmanFilament represents a filament type from Spoolman
type SpoolmanFilament struct {
	ID                   int                    `json:"id"`
	Registered           string                 `json:"registered"`
	Name                 string                 `json:"name"`
	Vendor               *SpoolmanVendor        `json:"vendor"`
	Material             string                 `json:"material"`
	Density              float64                `json:"density"`
	Diameter             float64                `json:"diameter"`
	Weight               float64                `json:"weight"`
	SpoolWeight          float64                `json:"spool_weight"`
	SettingsExtruderTemp int                    `json:"settings_extruder_temp"`
	SettingsBedTemp      int                    `json:"settings_bed_temp"`
	ColorHex             string                 `json:"color_hex"`
	MultiColorHexes      string                 `json:"multi_color_hexes"`     // comma-separated hex list (no #) for multi-color filament; color_hex still holds a primary color
	MultiColorDirection  string                 `json:"multi_color_direction"` // "coaxial" | "longitudinal" (not currently used for rendering)
	ExternalID           string                 `json:"external_id"`
	Extra                map[string]interface{} `json:"extra"`
	Archived             bool                   `json:"archived"`
}

// SpoolmanVendor represents a vendor from Spoolman
type SpoolmanVendor struct {
	ID         int                    `json:"id"`
	Registered string                 `json:"registered"`
	Name       string                 `json:"name"`
	ExternalID string                 `json:"external_id"`
	Extra      map[string]interface{} `json:"extra"`
	Archived   bool                   `json:"archived"`
}

// SpoolmanError represents an error response from Spoolman API
type SpoolmanError struct {
	Detail string `json:"detail"`
	Title  string `json:"title"`
	Type   string `json:"type"`
}

// NewSpoolmanClient creates a new Spoolman client
func NewSpoolmanClient(baseURL string, timeout int, username, password string) *SpoolmanClient {
	return &SpoolmanClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: time.Duration(timeout) * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     30 * time.Second,
			},
		},
		username: username,
		password: password,
	}
}

// addAuthHeader adds Basic Authentication header to the request if both username and password are provided
func (c *SpoolmanClient) addAuthHeader(req *http.Request) {
	if c.username != "" && c.password != "" {
		auth := c.username + ":" + c.password
		encoded := base64.StdEncoding.EncodeToString([]byte(auth))
		req.Header.Set("Authorization", "Basic "+encoded)
	}
}

// handleAPIError handles API error responses from Spoolman
func (c *SpoolmanClient) handleAPIError(resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("error reading error response body: %w", err)
	}

	// Try to parse as Spoolman error format
	var spoolmanErr SpoolmanError
	if err := json.Unmarshal(body, &spoolmanErr); err == nil && spoolmanErr.Detail != "" {
		return fmt.Errorf("spoolman API error (HTTP %d): %s - %s", resp.StatusCode, spoolmanErr.Title, spoolmanErr.Detail)
	}

	// Fallback to generic error
	return fmt.Errorf("spoolman API error (HTTP %d): %s", resp.StatusCode, string(body))
}

// normalizeSpoolData normalizes spool data to extract information from nested structures
func (c *SpoolmanClient) normalizeSpoolData(spool SpoolmanSpool) SpoolmanSpool {
	// Extract data from nested filament and vendor structures
	if spool.Filament != nil {
		spool.Name = spool.Filament.Name
		spool.Material = spool.Filament.Material

		if spool.Filament.Vendor != nil {
			spool.Brand = spool.Filament.Vendor.Name
		}
	}

	// If name is still empty, create a default name
	if spool.Name == "" {
		spool.Name = fmt.Sprintf("Spool %d", spool.ID)
	}

	return spool
}

// getSpoolDisplayName returns the display name for sorting purposes
func (spool *SpoolmanSpool) getSpoolDisplayName() string {
	material := "Unknown Material"
	brand := "Unknown Brand"
	name := "Unnamed Spool"

	if spool.Filament != nil {
		if spool.Filament.Material != "" {
			material = spool.Filament.Material
		}
		if spool.Filament.Vendor != nil && spool.Filament.Vendor.Name != "" {
			brand = spool.Filament.Vendor.Name
		}
		if spool.Filament.Name != "" {
			name = spool.Filament.Name
		}
	}

	return fmt.Sprintf("%s - %s - %s", material, brand, name)
}

// GetAllSpools gets all filament spools from Spoolman
func (c *SpoolmanClient) GetAllSpools() ([]SpoolmanSpool, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/api/v1/spool", nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", err)
	}
	c.addAuthHeader(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error getting spools from Spoolman: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.handleAPIError(resp)
	}

	var spools []SpoolmanSpool
	if err := json.NewDecoder(resp.Body).Decode(&spools); err != nil {
		return nil, fmt.Errorf("error decoding spools from Spoolman: %w", err)
	}

	// Normalize spool data to extract information from nested structures
	for i := range spools {
		spools[i] = c.normalizeSpoolData(spools[i])
	}

	// Filter out archived spools and spools with 0g remaining weight
	filteredSpools := make([]SpoolmanSpool, 0, len(spools))
	for _, spool := range spools {
		if !spool.Archived && spool.RemainingWeight > 0 {
			filteredSpools = append(filteredSpools, spool)
		}
	}
	spools = filteredSpools

	// Sort spools: first alphabetically by display name, then by remaining weight (descending)
	sort.Slice(spools, func(i, j int) bool {
		// First sort by display name (Material - Brand - Name)
		nameI := spools[i].getSpoolDisplayName()
		nameJ := spools[j].getSpoolDisplayName()

		if nameI != nameJ {
			return nameI < nameJ
		}

		// If display names are the same, sort by remaining weight (ascending - use less filament first)
		return spools[i].RemainingWeight < spools[j].RemainingWeight
	})

	return spools, nil
}

// GetAllFilaments gets all filament types from Spoolman
func (c *SpoolmanClient) GetAllFilaments() ([]SpoolmanFilament, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/api/v1/filament", nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", err)
	}
	c.addAuthHeader(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error getting filaments from Spoolman: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.handleAPIError(resp)
	}

	var filaments []SpoolmanFilament
	if err := json.NewDecoder(resp.Body).Decode(&filaments); err != nil {
		return nil, fmt.Errorf("error decoding filaments from Spoolman: %w", err)
	}

	// Filter out archived filaments
	filteredFilaments := make([]SpoolmanFilament, 0, len(filaments))
	for _, filament := range filaments {
		if !filament.Archived {
			filteredFilaments = append(filteredFilaments, filament)
		}
	}
	filaments = filteredFilaments

	// Sort filaments by ID
	sort.Slice(filaments, func(i, j int) bool {
		return filaments[i].ID < filaments[j].ID
	})

	return filaments, nil
}

// patchJSON sends a PATCH with a JSON body to the given API path (relative to
// the Spoolman base URL) and verifies a 200 response. opDesc names the
// operation for error messages, e.g. "updating spool 3".
func (c *SpoolmanClient) patchJSON(path string, data map[string]interface{}, opDesc string) error {
	_, err := c.patchJSONResult(path, data, opDesc)
	return err
}

// patchJSONResult is patchJSON for the few endpoints whose response body carries
// information we act on (e.g. the bulk field update's spools_updated count).
func (c *SpoolmanClient) patchJSONResult(path string, data map[string]interface{}, opDesc string) ([]byte, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("error marshaling data for %s: %w", opDesc, err)
	}

	req, err := http.NewRequest("PATCH", c.baseURL+path, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("error creating PATCH request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.addAuthHeader(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error %s in Spoolman: %w", opDesc, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.handleAPIError(resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response for %s: %w", opDesc, err)
	}
	return body, nil
}

// UpdateSpool updates spool information (used for filament usage tracking)
func (c *SpoolmanClient) UpdateSpool(spoolID int, data map[string]interface{}) error {
	return c.patchJSON(fmt.Sprintf("/api/v1/spool/%d", spoolID), data,
		fmt.Sprintf("updating spool %d", spoolID))
}

// GetSpool fetches a single spool by ID from Spoolman
func (c *SpoolmanClient) GetSpool(spoolID int) (*SpoolmanSpool, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/spool/%d", c.baseURL, spoolID), nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", err)
	}
	c.addAuthHeader(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error getting spool %d from Spoolman: %w", spoolID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("spool %d not found in Spoolman: %w", spoolID, c.handleAPIError(resp))
	}

	var spool SpoolmanSpool
	if err := json.NewDecoder(resp.Body).Decode(&spool); err != nil {
		return nil, fmt.Errorf("error decoding spool %d from Spoolman: %w", spoolID, err)
	}
	spool = c.normalizeSpoolData(spool)
	return &spool, nil
}

// UpdateSpoolUsage updates spool used weight based on usage (core bridge functionality)
func (c *SpoolmanClient) UpdateSpoolUsage(spoolID int, filamentUsed float64) error {
	spoolPtr, err := c.GetSpool(spoolID)
	if err != nil {
		return err
	}
	spool := *spoolPtr

	// Calculate new used weight
	newUsedWeight := spool.UsedWeight + filamentUsed
	currentTime := time.Now().UTC().Format(time.RFC3339)

	// Update used_weight and timestamps
	updateData := map[string]interface{}{
		"used_weight": newUsedWeight,
		"last_used":   currentTime,
	}

	// Set first_used if it's not already set
	if spool.FirstUsed == "" {
		updateData["first_used"] = currentTime
	}

	if err := c.UpdateSpool(spoolID, updateData); err != nil {
		return fmt.Errorf("failed to update spool %d: %w", spoolID, err)
	}

	log.Printf("Updated spool %d: used_weight %.2fg -> %.2fg (added %.2fg)",
		spoolID, spool.UsedWeight, newUsedWeight, filamentUsed)

	return nil
}

// TestConnection tests the connection to Spoolman
func (c *SpoolmanClient) TestConnection() error {
	req, err := http.NewRequest("GET", c.baseURL+"/api/v1/info", nil)
	if err != nil {
		return fmt.Errorf("error creating request: %w", err)
	}
	c.addAuthHeader(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("error testing connection to Spoolman: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return c.handleAPIError(resp)
	}

	return nil
}

// SpoolmanLocation represents a location from Spoolman
type SpoolmanLocation struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Comment  string `json:"comment"`
	Archived bool   `json:"archived"`
}

// GetLocations returns Spoolman's locations as reported by /api/v1/location,
// i.e. the distinct location strings currently referenced by spools. This
// mirrors what the Spoolman v0.26 UI itself shows: locations are just strings
// on spools, not first-class entities. The legacy `locations` setting is
// intentionally ignored — v0.26 no longer reads or writes it, so any
// predefined, spool-less entries there are invisible in Spoolman too.
func (c *SpoolmanClient) GetLocations() ([]SpoolmanLocation, error) {
	listLocs, err := c.getLocationsFromList()
	if err != nil {
		return nil, fmt.Errorf("failed to get locations from Spoolman: %w", err)
	}

	return listLocs, nil
}

// getLocationsFromList reads locations from Spoolman's GET /api/v1/location.
// The endpoint's response shape has varied across Spoolman versions, so we try
// several: an array of objects, a {data|results:[...]} wrapper, and a plain
// array of name strings (v0.26). Whichever parses, the names are normalized in
// one place: blank/whitespace names become "Unassigned", mirroring Spoolman's
// UI, which groups spool-less/empty locations under that label.
func (c *SpoolmanClient) getLocationsFromList() ([]SpoolmanLocation, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/api/v1/location", nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", err)
	}
	c.addAuthHeader(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error getting locations from Spoolman: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.handleAPIError(resp)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading locations response from Spoolman: %w", err)
	}

	var locations []SpoolmanLocation
	parsed := false

	// 1) Standard array of objects: [{"name":"...","id":1,...}, ...]
	if err := json.Unmarshal(bodyBytes, &locations); err == nil {
		parsed = true
	}

	// 2) Wrapped: {"data":[...]} or {"results":[...]}
	if !parsed {
		var wrapper struct {
			Data    []SpoolmanLocation `json:"data"`
			Results []SpoolmanLocation `json:"results"`
		}
		if err := json.Unmarshal(bodyBytes, &wrapper); err == nil {
			switch {
			case len(wrapper.Data) > 0:
				locations = wrapper.Data
				parsed = true
			case len(wrapper.Results) > 0:
				locations = wrapper.Results
				parsed = true
			}
		}
	}

	// 3) Plain array of names: ["Closet Shelf", "", ...] (Spoolman v0.26)
	if !parsed {
		var names []string
		if err := json.Unmarshal(bodyBytes, &names); err == nil {
			locations = make([]SpoolmanLocation, 0, len(names))
			for _, n := range names {
				locations = append(locations, SpoolmanLocation{Name: n})
			}
			parsed = true
		}
	}

	if !parsed {
		snippet := string(bodyBytes)
		if len(snippet) > 300 {
			snippet = snippet[:300] + "..."
		}
		log.Printf("Spoolman /location unexpected JSON. Snippet: %s", snippet)
		return nil, fmt.Errorf("error decoding locations from Spoolman: unexpected JSON shape")
	}

	// Single normalization pass, applied no matter which shape parsed:
	// blank names -> "Unassigned", and dedupe by final name so an "" and a
	// literal "Unassigned" (or two blanks) don't produce duplicates.
	normalized := make([]SpoolmanLocation, 0, len(locations))
	seen := make(map[string]bool)
	for _, loc := range locations {
		if strings.TrimSpace(loc.Name) == "" {
			loc.Name = "Unassigned"
		}
		if seen[loc.Name] {
			continue
		}
		seen[loc.Name] = true
		normalized = append(normalized, loc)
	}
	return normalized, nil
}

// GetOrCreateLocation gets an existing location by name
// Note: Spoolman API does not support creating locations via POST.
// Locations must be created manually in Spoolman UI or are auto-created when referenced in spools.
func (c *SpoolmanClient) GetOrCreateLocation(name string) (*SpoolmanLocation, error) {
	// Get existing locations
	locations, err := c.GetLocations()
	if err != nil {
		return nil, fmt.Errorf("failed to get locations: %w", err)
	}

	// Look for existing location with this name
	for _, location := range locations {
		if location.Name == name {
			return &location, nil
		}
	}

	// Location doesn't exist in Spoolman
	// Spoolman API doesn't support POST to create locations - they must be created
	// manually in the UI or will be auto-created when referenced in a spool
	// Return a dummy location so the system can continue
	return &SpoolmanLocation{
		ID:   0, // Dummy ID - location doesn't exist yet
		Name: name,
	}, nil
}

// FindLocationByName searches for an existing location by name
func (c *SpoolmanClient) FindLocationByName(name string) (*SpoolmanLocation, error) {
	locations, err := c.GetLocations()
	if err != nil {
		return nil, fmt.Errorf("error getting locations: %w", err)
	}

	for _, location := range locations {
		if location.Name == name {
			return &location, nil
		}
	}

	return nil, nil // Location not found
}

// UpdateSpoolLocation updates a spool's location in Spoolman using text-based location field
func (c *SpoolmanClient) UpdateSpoolLocation(spoolID int, locationName string) error {
	// Use text-based location assignment - Spoolman will create the location if it doesn't exist
	err := c.patchJSON(fmt.Sprintf("/api/v1/spool/%d", spoolID),
		map[string]interface{}{"location": locationName},
		fmt.Sprintf("updating spool %d location", spoolID))
	if err != nil {
		return err
	}
	log.Printf("Successfully updated spool %d to location '%s' (text-based)", spoolID, locationName)
	return nil
}

// UpdateLocationByName renames a location by rewriting the `location` string on
// every spool that currently carries oldName. This mirrors Spoolman's own
// dashboard group-rename (PATCH /api/v1/spool/field/location): locations are
// just strings on spools, not first-class entities, so a rename is a bulk
// field update.
func (c *SpoolmanClient) UpdateLocationByName(oldName, newName string) error {
	// Keep the caller's spelling for logs and errors; the wire values below are
	// the mapped ones.
	displayOld, displayNew := oldName, newName

	// Map the UI's "Unassigned" back to the empty string Spoolman stores.
	if oldName == "Unassigned" {
		oldName = ""
	}
	if newName == "Unassigned" {
		newName = ""
	}

	body, err := c.patchJSONResult("/api/v1/spool/field/location",
		map[string]interface{}{"value": oldName, "new_value": newName},
		fmt.Sprintf("renaming location '%s' to '%s'", displayOld, displayNew))
	if err != nil {
		return err
	}

	// Spoolman answers 200 with a count even when the old name matched nothing.
	// A rename that moved no spools changed nothing, so report it rather than
	// telling the user it worked.
	var result struct {
		SpoolsUpdated *int `json:"spools_updated"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		// Unrecognized shape: the PATCH itself succeeded, so don't fail the rename.
		log.Printf("Warning: could not parse rename response for location '%s': %v", displayOld, err)
		return nil
	}
	if result.SpoolsUpdated != nil && *result.SpoolsUpdated == 0 {
		return fmt.Errorf("no spools are in location '%s'", displayOld)
	}

	log.Printf("Successfully renamed Spoolman location '%s' to '%s'", displayOld, displayNew)
	return nil
}
