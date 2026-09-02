package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// NewSpoolmanClient creates a new Spoolman client.
//
// The base URL is normalized here so every request path below can be built by
// plain concatenation. A URL typed with a trailing slash would otherwise
// produce "//api/v1/spool", which Spoolman does not route to its API: it falls
// through to the web UI and answers 200 with HTML. Only trailing slashes and
// surrounding whitespace are removed. Any path is kept, because Spoolman is
// legitimately hosted under a subpath behind a reverse proxy.
func NewSpoolmanClient(baseURL string, timeout int, username, password string) *SpoolmanClient {
	return &SpoolmanClient{
		baseURL: normalizeSpoolmanBaseURL(baseURL),
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

// SpoolmanAPIError is a non-2xx response from Spoolman. It carries the status
// code so callers can tell apart failures that mean different things — most
// usefully a 404, which on a versioned endpoint means "this Spoolman is too old"
// rather than "the request was wrong". Callers that don't care can treat it as
// a plain error; the message is unchanged from what it always was.
type SpoolmanAPIError struct {
	StatusCode int
	Message    string
}

func (e *SpoolmanAPIError) Error() string { return e.Message }

// handleAPIError handles API error responses from Spoolman
func (c *SpoolmanClient) handleAPIError(resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("error reading error response body: %w", err)
	}

	// Try to parse as Spoolman error format
	var spoolmanErr SpoolmanError
	if err := json.Unmarshal(body, &spoolmanErr); err == nil && spoolmanErr.Detail != "" {
		return &SpoolmanAPIError{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("spoolman API error (HTTP %d): %s - %s", resp.StatusCode, spoolmanErr.Title, spoolmanErr.Detail),
		}
	}

	// Fallback to generic error
	return &SpoolmanAPIError{
		StatusCode: resp.StatusCode,
		Message:    fmt.Sprintf("spoolman API error (HTTP %d): %s", resp.StatusCode, string(body)),
	}
}

// normalizeSpoolmanBaseURL trims whitespace and trailing slashes from a
// configured Spoolman URL. See NewSpoolmanClient for why that matters.
func normalizeSpoolmanBaseURL(baseURL string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/")
}

// looksLikeHTML reports whether a response body is markup rather than JSON.
// Spoolman only ever answers the API with JSON, so a body opening with "<" is
// its web UI, served because the request never reached the API at all.
func looksLikeHTML(body []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(body, " \t\r\n"), []byte("<"))
}

// decodeSpoolmanJSON decodes a Spoolman response into v, turning the most
// common misconfiguration into an error the user can act on.
//
// Spoolman serves its web UI from the same origin as its API and answers a path
// it does not recognise with that UI and HTTP 200. A base URL with a stray
// path, a doubled slash, or one pointing at a reverse proxy rather than
// Spoolman itself therefore does not fail cleanly: it succeeds, and the only
// symptom is encoding/json reporting `invalid character '<' looking for
// beginning of value`, which tells the user nothing about what is wrong.
func decodeSpoolmanJSON(body []byte, requestURL string, v interface{}) error {
	if looksLikeHTML(body) {
		return fmt.Errorf("Spoolman returned a web page instead of JSON for %s. "+
			"Check the Spoolman URL in settings: it should be the root of your Spoolman "+
			"instance, such as http://spoolman.local:7912, with no /api path on the end", requestURL)
	}
	return json.Unmarshal(body, v)
}

// readSpoolmanJSON reads a response body and decodes it, reporting an HTML page
// as the configuration error it is.
func readSpoolmanJSON(resp *http.Response, v interface{}) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("error reading response from Spoolman: %w", err)
	}
	return decodeSpoolmanJSON(body, resp.Request.URL.String(), v)
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
	if err := readSpoolmanJSON(resp, &spools); err != nil {
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
	if err := readSpoolmanJSON(resp, &filaments); err != nil {
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

// patchJSONResult is patchJSON for the endpoints whose response body carries
// something we act on, such as the bulk field update's spools_updated count.
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

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response for %s: %w", opDesc, err)
	}
	return respBody, nil
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
	if err := readSpoolmanJSON(resp, &spool); err != nil {
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

// TestConnection tests the connection to Spoolman.
//
// It checks the response body, not just the status code. Spoolman answers a
// path it does not route to its API with the web UI and HTTP 200, so a status
// check alone passes for a URL that no real call will work against - the test
// goes green and then every spool, filament and location request fails. The
// body has to actually be JSON for the connection to count as good.
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

	// Only the shape is checked, not any particular field: this has to keep
	// working across Spoolman versions, and anything that decodes as a JSON
	// object came from the API rather than the web UI.
	var info map[string]interface{}
	if err := readSpoolmanJSON(resp, &info); err != nil {
		return fmt.Errorf("connected to %s but it did not answer as Spoolman's API: %w", c.baseURL, err)
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

// GetLocations returns Spoolman's locations as reported by /api/v1/location.
//
// It used to prefer the `locations` setting, falling back to /api/v1/location
// only when that request errored. On Spoolman v0.26 the setting request
// succeeds with an empty, never-set value ({"value":"[]","is_set":false},
// HTTP 200), so the fallback never ran and no locations were imported. The
// v0.26 client has no Locations page and never writes that setting — locations
// there are just the free-text `location` strings on spools — so the setting is
// not a source of truth to fall back to.
func (c *SpoolmanClient) GetLocations() ([]SpoolmanLocation, error) {
	return c.getLocationsFromList()
}

// getLocationsFromList reads locations from /api/v1/location, i.e. the distinct
// location strings currently referenced by spools.
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

	// Read full body so we can retry alternative shapes and log on error
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading locations response from Spoolman: %w", err)
	}

	// 1) Try standard array of objects
	var locations []SpoolmanLocation

	// An HTML body is a misconfigured URL, not an unrecognised JSON shape. Say
	// so before falling into the shape-guessing below, which would otherwise
	// report a web page as "unexpected JSON shape".
	if looksLikeHTML(bodyBytes) {
		return nil, decodeSpoolmanJSON(bodyBytes, resp.Request.URL.String(), &locations)
	}
	if err := json.Unmarshal(bodyBytes, &locations); err == nil {
		return withNonEmptyNames(locations), nil
	}

	// 2) Try { data: [...] } wrapper
	var dataWrapper struct {
		Data    []SpoolmanLocation `json:"data"`
		Results []SpoolmanLocation `json:"results"`
	}
	if err := json.Unmarshal(bodyBytes, &dataWrapper); err == nil {
		if len(dataWrapper.Data) > 0 {
			return withNonEmptyNames(dataWrapper.Data), nil
		}
		if len(dataWrapper.Results) > 0 {
			return withNonEmptyNames(dataWrapper.Results), nil
		}
	}

	// 3) Try simple array of names like ["Testing", ...] (Spoolman v0.26).
	// Build a fresh slice rather than appending to `locations`: the failed
	// attempt at shape 1 leaves it grown to the element count with zero-value
	// entries, and appending here would carry those blanks through.
	var names []string
	if err := json.Unmarshal(bodyBytes, &names); err == nil {
		named := make([]SpoolmanLocation, 0, len(names))
		for _, n := range names {
			named = append(named, SpoolmanLocation{Name: n})
		}
		return withNonEmptyNames(named), nil
	}

	// Log snippet for diagnostics and return error
	snippet := string(bodyBytes)
	if len(snippet) > 300 {
		snippet = snippet[:300] + "..."
	}
	log.Printf("Spoolman /location unexpected JSON. Snippet: %s", snippet)
	return nil, fmt.Errorf("error decoding locations from Spoolman: unexpected JSON shape")
}

// withNonEmptyNames drops locations whose name is empty or whitespace-only.
// Now that /api/v1/location is the only source, unassigned spools surface as a
// "" location, which is not a place anything can be filed under.
func withNonEmptyNames(locs []SpoolmanLocation) []SpoolmanLocation {
	out := make([]SpoolmanLocation, 0, len(locs))
	for _, loc := range locs {
		if strings.TrimSpace(loc.Name) == "" {
			continue
		}
		out = append(out, loc)
	}
	return out
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
// every spool that currently carries oldName.
//
// A location is not an entity in Spoolman — it is just a free-text field on a
// spool — so renaming one means a bulk field update, which is what Spoolman's
// own dashboard group-rename does. The previous implementation PATCHed
// /api/v1/location/{id}; that endpoint is gone in v0.26, and even where it
// existed it renamed a settings record without touching any spool, so the
// rename appeared to succeed and changed nothing.
//
// TODO(spoolman-compat): PATCH /api/v1/spool/field/location is new in v0.26.0
// and absent in v0.25.x, where this returns 404. The intended fallback is a
// per-spool loop — GET /api/v1/spool?location=<old>, then PATCH
// /api/v1/spool/{id} with {"location": new} for each, an endpoint that has
// existed since at least v0.21. renameLocationBulk below is the single seam
// where that version check and fallback belong; nothing else needs to change.
func (c *SpoolmanClient) UpdateLocationByName(oldName, newName string) error {
	updated, err := c.renameLocationBulk(oldName, newName)
	if err != nil {
		return err
	}

	// Spoolman answers 200 with a count even when the old name matched nothing.
	// A rename that moved no spools changed nothing, so say so rather than
	// reporting success.
	if updated == 0 {
		return fmt.Errorf("no spools are in location '%s'", oldName)
	}

	log.Printf("Successfully renamed Spoolman location '%s' to '%s' (%d spools)", oldName, newName, updated)
	return nil
}

// renameLocationBulk performs the rename and reports how many spools moved.
// It is the single seam for the version compatibility described on
// UpdateLocationByName: a pre-v0.26 fallback belongs here and nowhere else.
func (c *SpoolmanClient) renameLocationBulk(oldName, newName string) (int, error) {
	body, err := c.patchJSONResult("/api/v1/spool/field/location",
		map[string]interface{}{"value": oldName, "new_value": newName},
		fmt.Sprintf("renaming location '%s' to '%s'", oldName, newName))
	if err != nil {
		// A 404 here means the endpoint itself is missing, not that anything
		// about the request was wrong: it was added in Spoolman v0.26.0. Say so,
		// rather than surfacing a bare HTTP error the user cannot act on.
		var apiErr *SpoolmanAPIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return 0, fmt.Errorf("renaming locations requires Spoolman v0.26.0 or newer; this server does not support it")
		}
		return 0, err
	}

	var result struct {
		SpoolsUpdated *int `json:"spools_updated"`
	}

	// An HTML body is a misconfigured URL answered by Spoolman's web UI with
	// HTTP 200, not a response shape we failed to recognise. It has to be an
	// error: the tolerant fallback below would otherwise report a rename that
	// never reached the API as having moved a spool.
	if looksLikeHTML(body) {
		return 0, decodeSpoolmanJSON(body, c.baseURL+"/api/v1/spool/field/location", &result)
	}

	if err := json.Unmarshal(body, &result); err != nil || result.SpoolsUpdated == nil {
		// The PATCH itself succeeded, so don't fail the rename over a response
		// shape we don't recognize; report it as "moved something".
		log.Printf("Warning: could not read spools_updated when renaming '%s': %v", oldName, err)
		return 1, nil
	}
	return *result.SpoolsUpdated, nil
}
