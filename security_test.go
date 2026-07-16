package main

import (
	"net/http"
	"strings"
	"testing"
)

// TestConfigAPIMasksCredentials: stored secrets must never appear in the
// config GET response; a *_set flag reports their presence instead.
func TestConfigAPIMasksCredentials(t *testing.T) {
	ws, _, _ := newTestServer(t)
	ws.bridge.SetConfigValue(ConfigKeySpoolmanPassword, "hunter2")

	rec, body := doJSON(t, ws, http.MethodGet, "/api/config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("config GET: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatal("stored password leaked in config response")
	}
	if body[ConfigKeySpoolmanPassword] != "" {
		t.Errorf("password field should be masked, got %q", body[ConfigKeySpoolmanPassword])
	}
	if body[ConfigKeySpoolmanPassword+"_set"] != "true" {
		t.Errorf("password_set flag missing: %v", body[ConfigKeySpoolmanPassword+"_set"])
	}
}

// TestConfigUpdatePreservesMaskedPassword: an empty credential in the update
// payload (the masked round-trip) keeps the stored value; a non-empty one
// replaces it.
func TestConfigUpdatePreservesMaskedPassword(t *testing.T) {
	ws, _, _ := newTestServer(t)
	ws.bridge.SetConfigValue(ConfigKeySpoolmanPassword, "hunter2")

	// Masked round-trip: empty password submitted alongside other settings
	rec, _ := doJSON(t, ws, http.MethodPost, "/api/config", `{"spoolman_password":"","poll_interval":"15"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("config POST: %d %s", rec.Code, rec.Body.String())
	}
	if v, _ := ws.bridge.GetConfigValue(ConfigKeySpoolmanPassword); v != "hunter2" {
		t.Fatalf("masked round-trip wiped the password: %q", v)
	}
	if v, _ := ws.bridge.GetConfigValue(ConfigKeyPollInterval); v != "15" {
		t.Fatalf("non-sensitive value not updated: %q", v)
	}

	// Explicit replacement works
	rec, _ = doJSON(t, ws, http.MethodPost, "/api/config", `{"spoolman_password":"newpass"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("config POST: %d", rec.Code)
	}
	if v, _ := ws.bridge.GetConfigValue(ConfigKeySpoolmanPassword); v != "newpass" {
		t.Fatalf("explicit password update ignored: %q", v)
	}
}

// TestPrintersAPIMasksKeys: printer API keys must never be returned; the
// api_key_set flag reports whether one is stored.
func TestPrintersAPIMasksKeys(t *testing.T) {
	ws, _, _ := newTestServer(t)

	rec, body := doJSON(t, ws, http.MethodGet, "/api/printers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("printers GET: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "test-key") {
		t.Fatal("printer API key leaked in printers response")
	}
	printers := body["printers"].(map[string]interface{})
	p := printers["printer_test"].(map[string]interface{})
	if p["api_key"] != "" {
		t.Errorf("api_key should be masked, got %q", p["api_key"])
	}
	if p["api_key_set"] != true {
		t.Errorf("api_key_set flag: %v", p["api_key_set"])
	}
}

// TestPrinterUpdatePreservesKeyOnEmpty: the edit form's masked round-trip
// (empty api_key) keeps the stored key; submitting a new key replaces it.
func TestPrinterUpdatePreservesKeyOnEmpty(t *testing.T) {
	ws, _, _ := newTestServer(t)

	rec, _ := doJSON(t, ws, http.MethodPut, "/api/printers/printer_test",
		`{"name":"Renamed","ip_address":"127.0.0.1:9999","toolheads":2,"api_key":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	configs, _ := ws.bridge.GetAllPrinterConfigs()
	if configs["printer_test"].APIKey != "test-key" {
		t.Fatalf("empty api_key wiped the stored key: %q", configs["printer_test"].APIKey)
	}
	if configs["printer_test"].Name != "Renamed" || configs["printer_test"].Toolheads != 2 {
		t.Errorf("other fields not updated: %+v", configs["printer_test"])
	}

	rec, _ = doJSON(t, ws, http.MethodPut, "/api/printers/printer_test",
		`{"name":"Renamed","ip_address":"127.0.0.1:9999","toolheads":2,"api_key":"new-key"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d", rec.Code)
	}
	configs, _ = ws.bridge.GetAllPrinterConfigs()
	if configs["printer_test"].APIKey != "new-key" {
		t.Fatalf("explicit key update ignored: %q", configs["printer_test"].APIKey)
	}
}
