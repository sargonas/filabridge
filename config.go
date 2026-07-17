package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// PrinterConfig represents configuration for a single printer
type PrinterConfig struct {
	Name      string `json:"name"`
	IPAddress string `json:"ip_address"`
	APIKey    string `json:"api_key,omitempty"`
	Toolheads int    `json:"toolheads"`
}

// Config holds all configuration for the application
type Config struct {
	SpoolmanURL                  string
	SpoolmanUsername             string
	SpoolmanPassword             string
	PollInterval                 time.Duration
	DBFile                       string
	WebPort                      string
	PrusaLinkTimeout             int
	PrusaLinkFileDownloadTimeout int
	SpoolmanTimeout              int
	Printers                     map[string]PrinterConfig // Key is printer ID, value is printer config
}

// LoadConfig loads configuration from database
func LoadConfig(bridge *FilamentBridge) (*Config, error) {
	// Get configuration from database
	configValues, err := bridge.GetAllConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config from database: %w", err)
	}

	pollInterval := parseIntConfig(configValues, ConfigKeyPollInterval, DefaultPollInterval)
	prusaLinkTimeout := parseIntConfig(configValues, ConfigKeyPrusaLinkTimeout, PrusaLinkTimeout)
	prusaLinkFileDownloadTimeout := parseIntConfig(configValues, ConfigKeyPrusaLinkFileDownloadTimeout, PrusaLinkFileDownloadTimeout)
	spoolmanTimeout := parseIntConfig(configValues, ConfigKeySpoolmanTimeout, SpoolmanTimeout)

	config := &Config{
		SpoolmanURL:                  configValues[ConfigKeySpoolmanURL],
		SpoolmanUsername:             configValues[ConfigKeySpoolmanUsername],
		SpoolmanPassword:             configValues[ConfigKeySpoolmanPassword],
		PollInterval:                 time.Duration(pollInterval) * time.Second,
		DBFile:                       getDBFilePath(),
		WebPort:                      configValues[ConfigKeyWebPort],
		PrusaLinkTimeout:             prusaLinkTimeout,
		PrusaLinkFileDownloadTimeout: prusaLinkFileDownloadTimeout,
		SpoolmanTimeout:              spoolmanTimeout,
		Printers:                     make(map[string]PrinterConfig),
	}

	// Load printer configs directly from database without making API calls.
	// This prevents race conditions and timeouts during config loading; live
	// printer status is handled by the monitoring cycle.
	printerConfigs, err := bridge.GetAllPrinterConfigs()
	if err != nil {
		log.Printf("Error loading printer configs: %v", err)
	}
	for printerID, printerConfig := range printerConfigs {
		config.Printers[printerID] = printerConfig
	}

	// If no printers configured (or loading failed), add placeholder
	if len(config.Printers) == 0 {
		config.Printers["no_printers"] = PrinterConfig{Name: "No Printers Configured"}
	}

	return config, nil
}

// parseIntConfig returns the named config value as an int, or the default when
// the key is absent or not a valid integer.
func parseIntConfig(values map[string]string, key string, defaultValue int) int {
	if str, exists := values[key]; exists {
		if parsed, err := strconv.Atoi(str); err == nil {
			return parsed
		}
	}
	return defaultValue
}

// resolvePrinterName resolves printer name from config, with fallback to IP-based name
func resolvePrinterName(config PrinterConfig) string {
	if config.Name != "" {
		return config.Name
	}
	return fmt.Sprintf("Printer_%s", config.IPAddress)
}

// getDBFilePath returns the database file path, checking environment variable first
func getDBFilePath() string {
	if dbPath := os.Getenv("FILABRIDGE_DB_PATH"); dbPath != "" {
		return filepath.Join(dbPath, DefaultDBFileName)
	}
	return DefaultDBFileName
}
