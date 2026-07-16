package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// FilamentBridge manages the connection between PrusaLink and Spoolman
type FilamentBridge struct {
	config   *Config
	spoolman *SpoolmanClient
	db       *sql.DB
	// In-flight print state is persisted in the active_jobs table (survives
	// restarts); only the concurrency guard for the usage-recording path is kept in memory.
	processingPrints map[string]bool          // Guard against overlapping monitor cycles recording the same printer's usage
	printErrors      map[string]PrintError    // Store print processing errors
	offlinePrinters  map[string]bool          // Printers currently logged as offline (edge-triggered reachability logging)
	printerStates    map[string]string        // Last logged state per printer (edge-triggered state logging)
	offlineMutex     sync.Mutex               // Guards offlinePrinters and printerStates
	scanAttempts     map[string]int           // Header-scan attempts per printer+file (bounded retries)
	scanInFlight     map[string]bool          // Printers with a header scan currently running
	scanMutex        sync.Mutex               // Guards scanAttempts and scanInFlight
	runoutWarnings   map[string]RunoutWarning // Active low-filament warnings
	runoutChecked    map[string]int           // Runout check attempts per printer+job+toolhead+spool
	warnMutex        sync.RWMutex             // Guards runoutWarnings and runoutChecked
	errorMutex       sync.RWMutex
	mutex            sync.RWMutex
}

// ToolheadMapping represents a mapping between a printer toolhead and a spool
type ToolheadMapping struct {
	PrinterName string    `json:"printer_name"`
	ToolheadID  int       `json:"toolhead_id"`
	SpoolID     int       `json:"spool_id"`
	MappedAt    time.Time `json:"mapped_at"`
	DisplayName string    `json:"display_name,omitempty"` // Custom toolhead name or empty for default
}

// PrintHistory represents a record of filament usage
type PrintHistory struct {
	ID            int       `json:"id"`
	PrinterName   string    `json:"printer_name"`
	ToolheadID    int       `json:"toolhead_id"`
	SpoolID       int       `json:"spool_id"`
	FilamentUsed  float64   `json:"filament_used"`
	PrintStarted  time.Time `json:"print_started"`
	PrintFinished time.Time `json:"print_finished"`
	JobName       string    `json:"job_name"`
	Status        string    `json:"status"` // "completed" or "cancelled"
}

// PrintError represents a failed print processing attempt
type PrintError struct {
	ID           string    `json:"id"`
	PrinterName  string    `json:"printer_name"`
	Filename     string    `json:"filename"`
	Error        string    `json:"error"`
	Timestamp    time.Time `json:"timestamp"`
	Acknowledged bool      `json:"acknowledged"`
}

// RunoutWarning flags a print whose remaining filament requirement exceeds
// what is left on the mapped spool. Informational by default; when the pause
// toggle is on the print is paused and AutoPaused records that, so
// acknowledging can resume it.
type RunoutWarning struct {
	ID              string    `json:"id"`
	PrinterID       string    `json:"printer_id"`
	PrinterName     string    `json:"printer_name"`
	ToolheadID      int       `json:"toolhead_id"`
	SpoolID         int       `json:"spool_id"`
	SpoolName       string    `json:"spool_name"`
	JobID           int       `json:"job_id"`
	RequiredWeight  float64   `json:"required_weight"`  // grams still needed to finish the print
	RemainingWeight float64   `json:"remaining_weight"` // grams left on the mapped spool
	AutoPaused      bool      `json:"auto_paused"`
	Timestamp       time.Time `json:"timestamp"`
	Acknowledged    bool      `json:"acknowledged"`
}

// PrinterStatus represents the current status of all printers
type PrinterStatus struct {
	Printers         map[string]PrinterData             `json:"printers"`
	ToolheadMappings map[string]map[int]ToolheadMapping `json:"toolhead_mappings"`
	Timestamp        time.Time                          `json:"timestamp"`
}

// PrinterData represents data for a single printer
type PrinterData struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// NewFilamentBridge creates a new FilamentBridge instance
func NewFilamentBridge(config *Config) (*FilamentBridge, error) {
	bridge := &FilamentBridge{
		config:           config,
		spoolman:         NewSpoolmanClient(DefaultSpoolmanURL, SpoolmanTimeout, "", ""), // Default URL and timeout, will be updated
		processingPrints: make(map[string]bool),
		printErrors:      make(map[string]PrintError),
		offlinePrinters:  make(map[string]bool),
		printerStates:    make(map[string]string),
		scanAttempts:     make(map[string]int),
		scanInFlight:     make(map[string]bool),
		runoutWarnings:   make(map[string]RunoutWarning),
		runoutChecked:    make(map[string]int),
	}

	// Initialize database
	if err := bridge.initDatabase(); err != nil {
		return nil, fmt.Errorf("failed to initialize database: %w", err)
	}

	// Update Spoolman URL and timeout if config is provided
	if config != nil && config.SpoolmanURL != "" {
		bridge.spoolman = NewSpoolmanClient(config.SpoolmanURL, config.SpoolmanTimeout, config.SpoolmanUsername, config.SpoolmanPassword)
	}

	return bridge, nil
}

// initDatabase initializes the SQLite database
func (b *FilamentBridge) initDatabase() error {
	dbFile := DefaultDBFileName
	if b.config != nil && b.config.DBFile != "" {
		dbFile = b.config.DBFile
	}
	// Check for environment variable (path only, append filename)
	if envDBPath := os.Getenv("FILABRIDGE_DB_PATH"); envDBPath != "" {
		dbFile = filepath.Join(envDBPath, DefaultDBFileName)
	}

	// WAL mode survives power loss much better than the default rollback
	// journal (relevant for Raspberry Pi deployments) and allows concurrent
	// reads while writing. The busy timeout makes concurrent writers wait
	// instead of failing with SQLITE_BUSY. Note: WAL is unsuitable on network
	// filesystems (NFS/SMB); keep the database on local disk or a Docker volume.
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", dbFile))
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	b.db = db

	// Create tables
	createTables := []string{
		`CREATE TABLE IF NOT EXISTS configuration (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			description TEXT,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS printer_configs (
			printer_id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			ip_address TEXT NOT NULL,
			api_key TEXT,
			toolheads INTEGER DEFAULT 1,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS toolhead_mappings (
			printer_name TEXT,
			toolhead_id INTEGER,
			spool_id INTEGER,
			mapped_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (printer_name, toolhead_id)
		)`,
		`CREATE TABLE IF NOT EXISTS print_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			printer_name TEXT,
			toolhead_id INTEGER,
			spool_id INTEGER,
			filament_used REAL,
			print_started TIMESTAMP,
			print_finished TIMESTAMP,
			job_name TEXT,
			status TEXT NOT NULL DEFAULT 'completed'
		)`,
		`CREATE TABLE IF NOT EXISTS nfc_sessions (
			session_id TEXT PRIMARY KEY,
			spool_id INTEGER,
			printer_name TEXT,
			toolhead_id INTEGER,
			location_name TEXT,
			is_printer_location BOOLEAN,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			expires_at TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS toolhead_names (
			printer_id TEXT,
			toolhead_id INTEGER,
			display_name TEXT NOT NULL,
			PRIMARY KEY (printer_id, toolhead_id)
		)`,
		// active_jobs persists the in-flight print per printer so that print
		// detection survives a restart of the bridge mid-print.
		`CREATE TABLE IF NOT EXISTS active_jobs (
			printer_id TEXT PRIMARY KEY,
			job_id INTEGER NOT NULL DEFAULT 0,
			filename TEXT NOT NULL DEFAULT '',
			last_progress REAL NOT NULL DEFAULT 0,
			started_at TIMESTAMP NOT NULL,
			usage_json TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMP NOT NULL
		)`,
		// recorded_jobs is an idempotency ledger: a (printer_id, job_id) pair is
		// written once its filament usage has been recorded in Spoolman, so the same
		// job can never be double-counted (on double-detect or restart).
		`CREATE TABLE IF NOT EXISTS recorded_jobs (
			printer_id TEXT NOT NULL,
			job_id INTEGER NOT NULL,
			filename TEXT NOT NULL DEFAULT '',
			scale REAL NOT NULL DEFAULT 1,
			recorded_at TIMESTAMP NOT NULL,
			PRIMARY KEY (printer_id, job_id)
		)`,
	}

	// Pre-release builds stored a cosmetic printer model; drop the leftover
	// column from existing databases (it was display-only, nothing reads it).
	var modelCol string
	if err := b.db.QueryRow(`SELECT name FROM pragma_table_info('printer_configs') WHERE name='model'`).Scan(&modelCol); err == nil {
		if _, err := b.db.Exec(`ALTER TABLE printer_configs DROP COLUMN model`); err != nil {
			return fmt.Errorf("failed to drop model column: %w", err)
		}
	}

	// Pre-release builds named the recorded_jobs table billed_jobs; rename it
	// (and its billed_at column) in place before creating tables so existing
	// dedup history is preserved. Runs only when the old table exists.
	var legacyName string
	if err := b.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='billed_jobs'`).Scan(&legacyName); err == nil {
		if _, err := b.db.Exec(`ALTER TABLE billed_jobs RENAME TO recorded_jobs`); err != nil {
			return fmt.Errorf("failed to rename billed_jobs table: %w", err)
		}
		if _, err := b.db.Exec(`ALTER TABLE recorded_jobs RENAME COLUMN billed_at TO recorded_at`); err != nil {
			return fmt.Errorf("failed to rename billed_at column: %w", err)
		}
	}

	for _, query := range createTables {
		if _, err := b.db.Exec(query); err != nil {
			return fmt.Errorf("failed to create table: %w", err)
		}
	}

	// Databases created before the status column existed need it added in place;
	// rows from those versions predate cancelled-print tracking, so 'completed'
	// is the only value they could represent.
	if _, err := b.db.Exec(`ALTER TABLE print_history ADD COLUMN status TEXT NOT NULL DEFAULT 'completed'`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		return fmt.Errorf("failed to add status column to print_history: %w", err)
	}

	// Initialize default configuration
	if err := b.initializeDefaultConfig(); err != nil {
		return fmt.Errorf("failed to initialize default configuration: %w", err)
	}

	return nil
}

// initializeDefaultConfig sets up default configuration values
func (b *FilamentBridge) initializeDefaultConfig() error {
	defaultConfigs := map[string]string{
		ConfigKeyPrinterIPs:                      "", // Comma-separated list of printer IP addresses
		ConfigKeyAPIKey:                          "", // PrusaLink API key for authentication
		ConfigKeySpoolmanURL:                     DefaultSpoolmanURL,
		ConfigKeySpoolmanUsername:                "", // Spoolman basic auth username (optional)
		ConfigKeySpoolmanPassword:                "", // Spoolman basic auth password (optional)
		ConfigKeyPollInterval:                    fmt.Sprintf("%d", DefaultPollInterval),
		ConfigKeyWebPort:                         DefaultWebPort,
		ConfigKeyPrusaLinkTimeout:                fmt.Sprintf("%d", PrusaLinkTimeout),
		ConfigKeyPrusaLinkFileDownloadTimeout:    fmt.Sprintf("%d", PrusaLinkFileDownloadTimeout),
		ConfigKeySpoolmanTimeout:                 fmt.Sprintf("%d", SpoolmanTimeout),
		ConfigKeyAutoAssignPreviousSpoolEnabled:  "false", // Enable auto-assignment of previous spool to default location
		ConfigKeyAutoAssignPreviousSpoolLocation: "",      // Default location name for auto-assigned previous spools
		ConfigKeyPrintHistoryEnabled:             "true",  // Keep a local record of prints for the history tab
		ConfigKeyRunoutWarningEnabled:            "true",  // Warn when the mapped spool has less filament than the print needs
		ConfigKeyRunoutPauseEnabled:              "false", // Also pause the print when a low-filament warning fires
	}

	// Check if this is a fresh installation by checking if any config exists
	var totalCount int
	err := b.db.QueryRow("SELECT COUNT(*) FROM configuration").Scan(&totalCount)
	if err != nil {
		return fmt.Errorf("failed to check config existence: %w", err)
	}

	// Only insert defaults if this is a fresh installation
	if totalCount == 0 {
		for key, value := range defaultConfigs {
			_, err := b.db.Exec(
				"INSERT INTO configuration (key, value, description) VALUES (?, ?, ?)",
				key, value, getConfigDescription(key),
			)
			if err != nil {
				return fmt.Errorf("failed to insert default config %s: %w", key, err)
			}
		}
	}

	return nil
}

// getConfigDescription returns a description for a configuration key
func getConfigDescription(key string) string {
	descriptions := map[string]string{
		ConfigKeyPrinterIPs:                      "Comma-separated list of printer IP addresses for PrusaLink",
		ConfigKeyAPIKey:                          "PrusaLink API key for authentication",
		ConfigKeySpoolmanURL:                     "URL of Spoolman instance",
		ConfigKeySpoolmanUsername:                "Spoolman basic auth username (optional, leave empty if not using basic auth)",
		ConfigKeySpoolmanPassword:                "Spoolman basic auth password (optional, leave empty if not using basic auth)",
		ConfigKeyPollInterval:                    "Polling interval in seconds",
		ConfigKeyWebPort:                         "Port for web interface",
		ConfigKeyPrusaLinkTimeout:                "PrusaLink API timeout in seconds",
		ConfigKeyPrusaLinkFileDownloadTimeout:    "PrusaLink file download timeout in seconds",
		ConfigKeySpoolmanTimeout:                 "Spoolman API timeout in seconds",
		ConfigKeyAutoAssignPreviousSpoolEnabled:  "Enable automatic assignment of previous spool to default location when assigning new spool to toolhead",
		ConfigKeyAutoAssignPreviousSpoolLocation: "Default location name where previous spools will be automatically assigned (must exist as a location)",
		ConfigKeyPrintHistoryEnabled:             "Keep a local record of prints and show the Print History tab (usage is recorded in Spoolman either way)",
		ConfigKeyRunoutWarningEnabled:            "Show a dashboard warning when the mapped spool has less filament remaining than the print requires",
		ConfigKeyRunoutPauseEnabled:              "Also pause the print when a low-filament warning fires (acknowledging resumes it)",
	}
	if desc, exists := descriptions[key]; exists {
		return desc
	}
	return "Configuration value"
}

// GetConfigValue gets a configuration value from the database
func (b *FilamentBridge) GetConfigValue(key string) (string, error) {
	var value string
	err := b.db.QueryRow("SELECT value FROM configuration WHERE key = ?", key).Scan(&value)
	if err != nil {
		return "", fmt.Errorf("failed to get config value for %s: %w", key, err)
	}
	return value, nil
}

// SetConfigValue sets a configuration value in the database
func (b *FilamentBridge) SetConfigValue(key, value string) error {
	_, err := b.db.Exec(
		"INSERT OR REPLACE INTO configuration (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)",
		key, value,
	)
	if err != nil {
		return fmt.Errorf("failed to set config value for %s: %w", key, err)
	}
	return nil
}

// GetAllConfig gets all configuration values
func (b *FilamentBridge) GetAllConfig() (map[string]string, error) {
	rows, err := b.db.Query("SELECT key, value FROM configuration")
	if err != nil {
		return nil, fmt.Errorf("failed to get all config: %w", err)
	}
	defer rows.Close()

	config := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("failed to scan config row: %w", err)
		}
		config[key] = value
	}

	return config, nil
}

// GetAutoAssignPreviousSpoolEnabled gets whether auto-assignment of previous spool is enabled
func (b *FilamentBridge) GetAutoAssignPreviousSpoolEnabled() (bool, error) {
	value, err := b.GetConfigValue(ConfigKeyAutoAssignPreviousSpoolEnabled)
	if err != nil {
		// If key doesn't exist, return false (default)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return value == "true", nil
}

// SetAutoAssignPreviousSpoolEnabled sets whether auto-assignment of previous spool is enabled
func (b *FilamentBridge) SetAutoAssignPreviousSpoolEnabled(enabled bool) error {
	value := "false"
	if enabled {
		value = "true"
	}
	return b.SetConfigValue(ConfigKeyAutoAssignPreviousSpoolEnabled, value)
}

// GetAutoAssignPreviousSpoolLocation gets the default location name for auto-assigned previous spools
func (b *FilamentBridge) GetAutoAssignPreviousSpoolLocation() (string, error) {
	value, err := b.GetConfigValue(ConfigKeyAutoAssignPreviousSpoolLocation)
	if err != nil {
		// If key doesn't exist, return empty string (default)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return value, nil
}

// SetAutoAssignPreviousSpoolLocation sets the default location name for auto-assigned previous spools
func (b *FilamentBridge) SetAutoAssignPreviousSpoolLocation(location string) error {
	return b.SetConfigValue(ConfigKeyAutoAssignPreviousSpoolLocation, location)
}

// GetPrintHistoryEnabled reports whether local print history is kept and the
// history tab shown. Defaults to true (databases from before this setting
// existed have no row for it). Spoolman usage recording is unaffected either way.
func (b *FilamentBridge) GetPrintHistoryEnabled() (bool, error) {
	value, err := b.GetConfigValue(ConfigKeyPrintHistoryEnabled)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		return true, err
	}
	return value != "false", nil
}

// SetPrintHistoryEnabled sets whether local print history is kept
func (b *FilamentBridge) SetPrintHistoryEnabled(enabled bool) error {
	value := "false"
	if enabled {
		value = "true"
	}
	return b.SetConfigValue(ConfigKeyPrintHistoryEnabled, value)
}

// ClearPrintHistory deletes all stored print history entries. Does not touch
// the recorded_jobs dedup ledger, which is required for billing idempotency.
func (b *FilamentBridge) ClearPrintHistory() error {
	_, err := b.db.Exec("DELETE FROM print_history")
	if err != nil {
		return fmt.Errorf("failed to clear print history: %w", err)
	}
	log.Printf("Print history cleared")
	return nil
}

// GetRunoutWarningEnabled reports whether low-filament warnings are shown.
// Defaults to true when the key is missing (pre-feature databases).
func (b *FilamentBridge) GetRunoutWarningEnabled() (bool, error) {
	value, err := b.GetConfigValue(ConfigKeyRunoutWarningEnabled)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		return true, err
	}
	return value != "false", nil
}

// GetRunoutPauseEnabled reports whether a low-filament warning also pauses the
// print. Defaults to false; it is an opt-in on top of the warning toggle.
func (b *FilamentBridge) GetRunoutPauseEnabled() (bool, error) {
	value, err := b.GetConfigValue(ConfigKeyRunoutPauseEnabled)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return value == "true", nil
}

// GetRunoutWarnings returns all unacknowledged low-filament warnings
func (b *FilamentBridge) GetRunoutWarnings() []RunoutWarning {
	b.warnMutex.RLock()
	defer b.warnMutex.RUnlock()

	var warnings []RunoutWarning
	for _, w := range b.runoutWarnings {
		if !w.Acknowledged {
			warnings = append(warnings, w)
		}
	}
	return warnings
}

// AcknowledgeRunoutWarning dismisses a low-filament warning. If the warning
// auto-paused the print and the printer is still paused, the print is resumed
// first; a print the user already resumed at the printer is left alone.
func (b *FilamentBridge) AcknowledgeRunoutWarning(id string) error {
	b.warnMutex.RLock()
	w, exists := b.runoutWarnings[id]
	b.warnMutex.RUnlock()
	if !exists {
		return fmt.Errorf("runout warning not found: %s", id)
	}

	if w.AutoPaused && w.JobID != 0 {
		cfg := b.GetConfigSnapshot()
		if cfg != nil {
			if pc, ok := cfg.Printers[w.PrinterID]; ok {
				client := NewPrusaLinkClient(pc.IPAddress, pc.APIKey, cfg.PrusaLinkTimeout, cfg.PrusaLinkFileDownloadTimeout)
				status, err := client.GetStatus()
				if err != nil {
					return fmt.Errorf("could not check printer state before resuming: %w", err)
				}
				if status.Printer.State == StatePaused {
					if err := client.ResumeJob(w.JobID); err != nil {
						return fmt.Errorf("failed to resume print: %w", err)
					}
					log.Printf("Resumed job %d on %s after low-filament warning was acknowledged", w.JobID, w.PrinterName)
				}
			}
		}
	}

	b.warnMutex.Lock()
	w.Acknowledged = true
	b.runoutWarnings[id] = w
	b.warnMutex.Unlock()
	return nil
}

// maxRunoutCheckAttempts bounds Spoolman lookups per (job, toolhead, spool) so
// a transient Spoolman failure gets retried without polling it forever.
const maxRunoutCheckAttempts = 3

// runoutCheckDone is stored once a (job, toolhead, spool) combination has been
// conclusively checked, so it is never re-checked (and never re-warned).
const runoutCheckDone = 1000

// checkRunoutWarnings compares the print's remaining filament requirement per
// toolhead against the mapped spool's remaining weight and raises a warning
// (optionally pausing the print) when the spool will run short. Each
// (job, toolhead, spool) combination is checked once; remapping a toolhead to
// a different spool triggers a fresh check.
func (b *FilamentBridge) checkRunoutWarnings(printerID string, config PrinterConfig, client *PrusaLinkClient, aj *activeJob) {
	if len(aj.Usage) == 0 {
		return
	}
	enabled, err := b.GetRunoutWarningEnabled()
	if err != nil || !enabled {
		return
	}

	printerName := resolvePrinterName(config)
	remainingFraction := 1 - aj.LastProgress
	if remainingFraction < 0 {
		remainingFraction = 0
	}

	for toolheadID, totalGrams := range aj.Usage {
		spoolID, err := b.GetToolheadMapping(printerName, toolheadID)
		if err != nil || spoolID == 0 {
			continue
		}

		memoKey := fmt.Sprintf("%s|%d|%d|%d", printerID, aj.JobID, toolheadID, spoolID)
		b.warnMutex.Lock()
		attempts := b.runoutChecked[memoKey]
		if attempts >= maxRunoutCheckAttempts {
			b.warnMutex.Unlock()
			continue
		}
		b.runoutChecked[memoKey] = attempts + 1
		b.warnMutex.Unlock()

		spool, err := b.spoolman.GetSpool(spoolID)
		if err != nil {
			log.Printf("Warning: could not check spool %d for low filament (attempt %d): %v", spoolID, attempts+1, err)
			continue
		}

		// Conclusive check: never revisit this combination
		b.warnMutex.Lock()
		b.runoutChecked[memoKey] = runoutCheckDone
		b.warnMutex.Unlock()

		needed := totalGrams * remainingFraction
		if spool.RemainingWeight >= needed {
			continue
		}

		autoPaused := false
		if pauseEnabled, err := b.GetRunoutPauseEnabled(); err == nil && pauseEnabled && aj.JobID != 0 {
			if err := client.PauseJob(aj.JobID); err != nil {
				log.Printf("Warning: could not pause job %d after low-filament warning: %v", aj.JobID, err)
			} else {
				autoPaused = true
				log.Printf("Paused job %d on %s: spool %d has %.1fg remaining but the print needs ~%.1fg",
					aj.JobID, printerName, spoolID, spool.RemainingWeight, needed)
			}
		}

		id := fmt.Sprintf("runout_%s_%d_%d_%d", sanitizeErrorID(printerName), aj.JobID, toolheadID, time.Now().Unix())
		warning := RunoutWarning{
			ID:              id,
			PrinterID:       printerID,
			PrinterName:     printerName,
			ToolheadID:      toolheadID,
			SpoolID:         spoolID,
			SpoolName:       spool.Name,
			JobID:           aj.JobID,
			RequiredWeight:  needed,
			RemainingWeight: spool.RemainingWeight,
			AutoPaused:      autoPaused,
			Timestamp:       time.Now(),
		}
		b.warnMutex.Lock()
		b.runoutWarnings[id] = warning
		b.warnMutex.Unlock()

		log.Printf("Low filament warning for %s toolhead %d: spool %d (%s) has %.1fg remaining, print needs ~%.1fg",
			printerName, toolheadID, spoolID, spool.Name, spool.RemainingWeight, needed)
	}
}

// clearRunoutState drops unacknowledged warnings and check memos for a printer
// once its job ends, so stale warnings do not outlive the print they describe.
func (b *FilamentBridge) clearRunoutState(printerID string) {
	b.warnMutex.Lock()
	defer b.warnMutex.Unlock()
	for id, w := range b.runoutWarnings {
		if w.PrinterID == printerID {
			delete(b.runoutWarnings, id)
		}
	}
	prefix := printerID + "|"
	for key := range b.runoutChecked {
		if strings.HasPrefix(key, prefix) {
			delete(b.runoutChecked, key)
		}
	}
}

// GetAllPrinterConfigs gets all printer configurations
func (b *FilamentBridge) GetAllPrinterConfigs() (map[string]PrinterConfig, error) {
	rows, err := b.db.Query("SELECT printer_id, name, ip_address, api_key, toolheads FROM printer_configs")
	if err != nil {
		return nil, fmt.Errorf("failed to get printer configs: %w", err)
	}
	defer rows.Close()

	configs := make(map[string]PrinterConfig)
	for rows.Next() {
		var printerID, name, ipAddress, apiKey string
		var toolheads int
		if err := rows.Scan(&printerID, &name, &ipAddress, &apiKey, &toolheads); err != nil {
			return nil, fmt.Errorf("failed to scan printer config row: %w", err)
		}
		configs[printerID] = PrinterConfig{
			Name:      name,
			IPAddress: ipAddress,
			APIKey:    apiKey,
			Toolheads: toolheads,
		}
	}

	return configs, nil
}

// SavePrinterConfig saves a printer configuration
func (b *FilamentBridge) SavePrinterConfig(printerID string, config PrinterConfig) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	_, err := b.db.Exec(`
		INSERT OR REPLACE INTO printer_configs (printer_id, name, ip_address, api_key, toolheads)
		VALUES (?, ?, ?, ?, ?)
	`, printerID, config.Name, config.IPAddress, config.APIKey, config.Toolheads)
	if err != nil {
		return fmt.Errorf("failed to save printer config: %w", err)
	}
	return nil
}

// DeletePrinterConfig deletes a printer configuration
func (b *FilamentBridge) DeletePrinterConfig(printerID string) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	_, err := b.db.Exec("DELETE FROM printer_configs WHERE printer_id = ?", printerID)
	if err != nil {
		return fmt.Errorf("failed to delete printer config: %w", err)
	}
	return nil
}

// GetToolheadName gets the display name for a toolhead, or returns default "Toolhead {ID}"
func (b *FilamentBridge) GetToolheadName(printerID string, toolheadID int) (string, error) {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	var displayName string
	err := b.db.QueryRow(
		"SELECT display_name FROM toolhead_names WHERE printer_id = ? AND toolhead_id = ?",
		printerID, toolheadID,
	).Scan(&displayName)

	if err == sql.ErrNoRows {
		// Return default name if not found
		return fmt.Sprintf("Toolhead %d", toolheadID), nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to get toolhead name: %w", err)
	}

	return displayName, nil
}

// SetToolheadName sets the display name for a toolhead
func (b *FilamentBridge) SetToolheadName(printerID string, toolheadID int, name string) error {
	// Validate name is not empty
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("toolhead name cannot be empty")
	}

	// Get printer config to find printer name (before acquiring lock)
	printerConfigs, err := b.GetAllPrinterConfigs()
	if err != nil {
		return fmt.Errorf("failed to get printer configs: %w", err)
	}

	printerConfig, exists := printerConfigs[printerID]
	if !exists {
		return fmt.Errorf("printer %s not found", printerID)
	}

	printerName := printerConfig.Name

	// Get old toolhead name to calculate old location name (before acquiring lock)
	var oldDisplayName string
	oldName, err := b.GetToolheadName(printerID, toolheadID)
	if err == nil {
		oldDisplayName = oldName
	} else {
		oldDisplayName = fmt.Sprintf("Toolhead %d", toolheadID)
	}

	oldLocationName := fmt.Sprintf("%s - %s", printerName, oldDisplayName)
	newLocationName := fmt.Sprintf("%s - %s", printerName, name)

	// Update toolhead name in database
	b.mutex.Lock()
	_, err = b.db.Exec(
		"INSERT OR REPLACE INTO toolhead_names (printer_id, toolhead_id, display_name) VALUES (?, ?, ?)",
		printerID, toolheadID, name,
	)
	b.mutex.Unlock()

	if err != nil {
		return fmt.Errorf("failed to set toolhead name: %w", err)
	}

	// If location name changed, update Spoolman (outside of lock)
	if oldLocationName != newLocationName {
		// Get all spools from Spoolman
		spools, err := b.spoolman.GetAllSpools()
		if err != nil {
			log.Printf("Warning: Failed to get spools from Spoolman to update location names: %v", err)
		} else {
			// Find spools with the old location name and update them
			updatedCount := 0
			for _, spool := range spools {
				if spool.Location == oldLocationName {
					if err := b.spoolman.UpdateSpoolLocation(spool.ID, newLocationName); err != nil {
						log.Printf("Warning: Failed to update spool %d location from '%s' to '%s': %v", spool.ID, oldLocationName, newLocationName, err)
					} else {
						updatedCount++
					}
				}
			}

			// Ensure the new location exists in Spoolman
			if _, err := b.spoolman.GetOrCreateLocation(newLocationName); err != nil {
				log.Printf("Warning: Failed to create/verify location '%s' in Spoolman: %v", newLocationName, err)
			}

			if updatedCount > 0 {
				log.Printf("Updated %d spool(s) location from '%s' to '%s'", updatedCount, oldLocationName, newLocationName)
			}
		}
	}

	log.Printf("Set toolhead name for printer %s, toolhead %d: %s", printerID, toolheadID, name)
	return nil
}

// GetAllToolheadNames gets all toolhead display names for a printer
func (b *FilamentBridge) GetAllToolheadNames(printerID string) (map[int]string, error) {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	rows, err := b.db.Query(
		"SELECT toolhead_id, display_name FROM toolhead_names WHERE printer_id = ? ORDER BY toolhead_id",
		printerID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get toolhead names: %w", err)
	}
	defer rows.Close()

	names := make(map[int]string)
	for rows.Next() {
		var toolheadID int
		var displayName string
		if err := rows.Scan(&toolheadID, &displayName); err != nil {
			return nil, fmt.Errorf("failed to scan toolhead name row: %w", err)
		}
		names[toolheadID] = displayName
	}

	return names, nil
}

// GetConfigSnapshot returns a snapshot of the current config for safe iteration
func (b *FilamentBridge) GetConfigSnapshot() *Config {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	// Return a copy of the config to prevent iteration issues during updates
	if b.config == nil {
		return nil
	}

	// Create a shallow copy of the config
	configCopy := &Config{
		SpoolmanURL:                  b.config.SpoolmanURL,
		SpoolmanUsername:             b.config.SpoolmanUsername,
		SpoolmanPassword:             b.config.SpoolmanPassword,
		PollInterval:                 b.config.PollInterval,
		LocationSyncInterval:         b.config.LocationSyncInterval,
		DBFile:                       b.config.DBFile,
		WebPort:                      b.config.WebPort,
		PrusaLinkTimeout:             b.config.PrusaLinkTimeout,
		PrusaLinkFileDownloadTimeout: b.config.PrusaLinkFileDownloadTimeout,
		SpoolmanTimeout:              b.config.SpoolmanTimeout,
		Printers:                     make(map[string]PrinterConfig),
	}

	// Copy printer configs
	for id, printer := range b.config.Printers {
		configCopy.Printers[id] = printer
	}

	return configCopy
}

// ReloadConfig reloads the configuration from the database
func (b *FilamentBridge) ReloadConfig() error {
	// Load config outside the lock to minimize lock time
	config, err := LoadConfig(b)
	if err != nil {
		return fmt.Errorf("failed to reload config: %w", err)
	}

	// Only lock briefly to swap the config pointer and recreate SpoolmanClient
	b.mutex.Lock()
	b.config = config
	if config.SpoolmanURL != "" {
		b.spoolman = NewSpoolmanClient(config.SpoolmanURL, config.SpoolmanTimeout, config.SpoolmanUsername, config.SpoolmanPassword)
	}
	b.mutex.Unlock()

	return nil
}

// IsFirstRun checks if this is the first time the application is running
func (b *FilamentBridge) IsFirstRun() (bool, error) {
	var count int
	err := b.db.QueryRow("SELECT COUNT(*) FROM printer_configs").Scan(&count)
	if err != nil {
		return false, fmt.Errorf("failed to check first run status: %w", err)
	}

	// If no printers are configured, this is a first run
	return count == 0, nil
}

// UpdateConfig updates the bridge configuration
func (b *FilamentBridge) UpdateConfig(config *Config) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	b.config = config
	b.spoolman = NewSpoolmanClient(config.SpoolmanURL, config.SpoolmanTimeout, config.SpoolmanUsername, config.SpoolmanPassword)

	return nil
}

// GetToolheadMapping gets spool ID mapped to a specific toolhead
func (b *FilamentBridge) GetToolheadMapping(printerName string, toolheadID int) (int, error) {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	var spoolID int
	err := b.db.QueryRow(
		"SELECT spool_id FROM toolhead_mappings WHERE printer_name = ? AND toolhead_id = ?",
		printerName, toolheadID,
	).Scan(&spoolID)

	if err == sql.ErrNoRows {
		return 0, nil // No mapping found
	}
	if err != nil {
		return 0, fmt.Errorf("failed to get toolhead mapping: %w", err)
	}

	return spoolID, nil
}

// SetToolheadMapping maps a spool to a specific toolhead
func (b *FilamentBridge) SetToolheadMapping(printerName string, toolheadID int, spoolID int) error {
	b.mutex.Lock()

	// Get the previous spool ID before replacing it (for auto-assignment feature)
	var previousSpoolID int
	err := b.db.QueryRow(
		"SELECT spool_id FROM toolhead_mappings WHERE printer_name = ? AND toolhead_id = ?",
		printerName, toolheadID,
	).Scan(&previousSpoolID)
	if err != nil && err != sql.ErrNoRows {
		b.mutex.Unlock()
		return fmt.Errorf("failed to get previous spool mapping: %w", err)
	}
	// If no previous mapping exists, previousSpoolID will be 0

	// Check if this spool is already assigned to a different toolhead
	rows, err := b.db.Query(
		"SELECT printer_name, toolhead_id FROM toolhead_mappings WHERE spool_id = ? AND NOT (printer_name = ? AND toolhead_id = ?)",
		spoolID, printerName, toolheadID,
	)
	if err != nil {
		b.mutex.Unlock()
		return fmt.Errorf("failed to check existing spool assignments: %w", err)
	}
	defer rows.Close()

	// If we find any rows, this spool is already assigned elsewhere
	if rows.Next() {
		var existingPrinterName string
		var existingToolheadID int
		if err := rows.Scan(&existingPrinterName, &existingToolheadID); err != nil {
			b.mutex.Unlock()
			return fmt.Errorf("failed to scan existing assignment: %w", err)
		}
		b.mutex.Unlock()
		return fmt.Errorf("spool %d is already assigned to %s toolhead %d", spoolID, existingPrinterName, existingToolheadID)
	}

	_, err = b.db.Exec(
		"INSERT OR REPLACE INTO toolhead_mappings (printer_name, toolhead_id, spool_id, mapped_at) VALUES (?, ?, ?, ?)",
		printerName, toolheadID, spoolID, time.Now(),
	)
	if err != nil {
		b.mutex.Unlock()
		return fmt.Errorf("failed to set toolhead mapping: %w", err)
	}

	log.Printf("Mapped %s toolhead %d to spool %d", printerName, toolheadID, spoolID)

	// Check if auto-assign feature is enabled and we have a previous spool to assign
	enabled, err := b.GetAutoAssignPreviousSpoolEnabled()
	if err != nil {
		log.Printf("Warning: Failed to check auto-assign previous spool setting: %v", err)
		b.mutex.Unlock()
		return nil // Don't fail the assignment if we can't check the setting
	}

	// Unlock before potentially calling AssignSpoolToLocation (which may need locks)
	b.mutex.Unlock()

	// Update the newly loaded spool's location in Spoolman
	b.updateSpoolLocationForToolhead(printerName, toolheadID, spoolID)

	if enabled && previousSpoolID > 0 && previousSpoolID != spoolID {
		// Get the configured default location
		locationName, err := b.GetAutoAssignPreviousSpoolLocation()
		if err != nil {
			log.Printf("Warning: Failed to get auto-assign previous spool location setting: %v", err)
			return nil // Don't fail the assignment
		}

		if locationName != "" {
			// Verify the location exists in Spoolman
			location, err := b.spoolman.FindLocationByName(locationName)
			if err != nil || location == nil {
				log.Printf("Warning: Auto-assign previous spool location '%s' does not exist, skipping auto-assignment of spool %d", locationName, previousSpoolID)
				return nil // Don't fail the assignment
			}

			// Assign the previous spool to the default location
			// Use isPrinterLocation = false since this is a storage location
			if err := b.AssignSpoolToLocation(previousSpoolID, "", 0, locationName, false); err != nil {
				log.Printf("Warning: Failed to auto-assign previous spool %d to location '%s': %v", previousSpoolID, locationName, err)
				// Don't fail the original assignment if auto-assignment fails
			} else {
				log.Printf("Auto-assigned previous spool %d to location '%s'", previousSpoolID, locationName)
			}
		}
	}

	return nil
}

// updateSpoolLocationForToolhead sets a spool's Spoolman location to
// "{PrinterName} - {ToolheadDisplayName}". Failures are logged but
// never propagated so the toolhead mapping itself is not rolled back.
func (b *FilamentBridge) updateSpoolLocationForToolhead(printerName string, toolheadID int, spoolID int) {
	displayName := fmt.Sprintf("Toolhead %d", toolheadID)
	printerConfigs, err := b.GetAllPrinterConfigs()
	if err == nil {
		for printerID, printerConfig := range printerConfigs {
			if printerConfig.Name == printerName {
				if name, err := b.GetToolheadName(printerID, toolheadID); err == nil {
					displayName = name
				}
				break
			}
		}
	}

	locationName := fmt.Sprintf("%s - %s", printerName, displayName)

	// Move any other spools currently at this location to the default storage location.
	b.evictSpoolsFromLocation(locationName, spoolID)

	if err := b.spoolman.UpdateSpoolLocation(spoolID, locationName); err != nil {
		log.Printf("Warning: Failed to update Spoolman location for spool %d to '%s': %v", spoolID, locationName, err)
	}
}

// evictSpoolsFromLocation moves any spools at the given location (except excludeSpoolID)
// to the auto-assign default storage location, or clears their location if unconfigured.
func (b *FilamentBridge) evictSpoolsFromLocation(locationName string, excludeSpoolID int) {
	spools, err := b.spoolman.GetAllSpools()
	if err != nil {
		log.Printf("Warning: Failed to fetch spools to check location '%s': %v", locationName, err)
		return
	}

	destLocation := ""
	enabled, err := b.GetAutoAssignPreviousSpoolEnabled()
	if err == nil && enabled {
		if name, err := b.GetAutoAssignPreviousSpoolLocation(); err == nil && name != "" {
			if loc, err := b.spoolman.FindLocationByName(name); err == nil && loc != nil {
				destLocation = name
			}
		}
	}

	for _, spool := range spools {
		if spool.ID == excludeSpoolID || spool.Location != locationName {
			continue
		}
		if err := b.spoolman.UpdateSpoolLocation(spool.ID, destLocation); err != nil {
			log.Printf("Warning: Failed to move spool %d from '%s': %v", spool.ID, locationName, err)
		} else if destLocation != "" {
			log.Printf("Moved spool %d from '%s' to '%s'", spool.ID, locationName, destLocation)
		} else {
			log.Printf("Cleared location for spool %d (was '%s')", spool.ID, locationName)
		}
	}
}

// GetToolheadMappings gets all toolhead mappings for a printer
func (b *FilamentBridge) GetToolheadMappings(printerName string) (map[int]ToolheadMapping, error) {
	rows, err := b.db.Query(
		"SELECT toolhead_id, spool_id, mapped_at FROM toolhead_mappings WHERE printer_name = ?",
		printerName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	mappings := make(map[int]ToolheadMapping)
	for rows.Next() {
		var toolheadID, spoolID int
		var mappedAt time.Time
		if err := rows.Scan(&toolheadID, &spoolID, &mappedAt); err != nil {
			return nil, err
		}
		mappings[toolheadID] = ToolheadMapping{
			PrinterName: printerName,
			ToolheadID:  toolheadID,
			SpoolID:     spoolID,
			MappedAt:    mappedAt,
		}
	}

	return mappings, nil
}

// GetAllToolheadMappings gets all toolhead mappings across all printers
func (b *FilamentBridge) GetAllToolheadMappings() (map[string]map[int]ToolheadMapping, error) {
	rows, err := b.db.Query(
		"SELECT printer_name, toolhead_id, spool_id, mapped_at FROM toolhead_mappings ORDER BY printer_name, toolhead_id",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	mappings := make(map[string]map[int]ToolheadMapping)
	for rows.Next() {
		var printerName string
		var toolheadID, spoolID int
		var mappedAt time.Time
		if err := rows.Scan(&printerName, &toolheadID, &spoolID, &mappedAt); err != nil {
			return nil, err
		}

		if mappings[printerName] == nil {
			mappings[printerName] = make(map[int]ToolheadMapping)
		}

		mappings[printerName][toolheadID] = ToolheadMapping{
			PrinterName: printerName,
			ToolheadID:  toolheadID,
			SpoolID:     spoolID,
			MappedAt:    mappedAt,
		}
	}

	return mappings, nil
}

// UnmapToolhead removes a spool mapping from a toolhead
func (b *FilamentBridge) UnmapToolhead(printerName string, toolheadID int) error {
	b.mutex.Lock()

	var spoolID int
	err := b.db.QueryRow(
		"SELECT spool_id FROM toolhead_mappings WHERE printer_name = ? AND toolhead_id = ?",
		printerName, toolheadID,
	).Scan(&spoolID)
	if err != nil && err != sql.ErrNoRows {
		b.mutex.Unlock()
		return fmt.Errorf("failed to query toolhead mapping: %w", err)
	}

	_, err = b.db.Exec(
		"DELETE FROM toolhead_mappings WHERE printer_name = ? AND toolhead_id = ?",
		printerName, toolheadID,
	)
	if err != nil {
		b.mutex.Unlock()
		return fmt.Errorf("failed to unmap toolhead: %w", err)
	}

	b.mutex.Unlock()
	log.Printf("Unmapped %s toolhead %d", printerName, toolheadID)

	if spoolID > 0 {
		locationName := ""
		enabled, err := b.GetAutoAssignPreviousSpoolEnabled()
		if err == nil && enabled {
			locationName, _ = b.GetAutoAssignPreviousSpoolLocation()
			if locationName != "" {
				loc, err := b.spoolman.FindLocationByName(locationName)
				if err != nil || loc == nil {
					log.Printf("Warning: Auto-assign location '%s' does not exist, clearing spool %d location", locationName, spoolID)
					locationName = ""
				}
			}
		}
		if err := b.spoolman.UpdateSpoolLocation(spoolID, locationName); err != nil {
			log.Printf("Warning: Failed to update Spoolman location for spool %d: %v", spoolID, err)
		}
	}

	return nil
}

// LogPrintUsage logs filament usage for a print job. printStarted is the time
// the print actually began (captured when the job was first seen printing); a
// zero value falls back to now. status is "completed" or "cancelled".
func (b *FilamentBridge) LogPrintUsage(printerName string, toolheadID int, spoolID int, filamentUsed float64, jobName string, printStarted time.Time, status string) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	if printStarted.IsZero() {
		printStarted = time.Now()
	}

	_, err := b.db.Exec(
		"INSERT INTO print_history (printer_name, toolhead_id, spool_id, filament_used, print_started, print_finished, job_name, status) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		printerName, toolheadID, spoolID, filamentUsed, printStarted, time.Now(), jobName, status,
	)
	if err != nil {
		return fmt.Errorf("failed to log print usage: %w", err)
	}

	return nil
}

// GetPrintHistory returns the most recent print history entries, newest first.
func (b *FilamentBridge) GetPrintHistory(limit int) ([]PrintHistory, error) {
	rows, err := b.db.Query(
		`SELECT id, printer_name, toolhead_id, spool_id, filament_used, print_started, print_finished, job_name, status
		 FROM print_history ORDER BY print_finished DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get print history: %w", err)
	}
	defer rows.Close()

	history := make([]PrintHistory, 0)
	for rows.Next() {
		var h PrintHistory
		if err := rows.Scan(&h.ID, &h.PrinterName, &h.ToolheadID, &h.SpoolID, &h.FilamentUsed,
			&h.PrintStarted, &h.PrintFinished, &h.JobName, &h.Status); err != nil {
			return nil, fmt.Errorf("failed to scan print history row: %w", err)
		}
		history = append(history, h)
	}

	return history, rows.Err()
}

// MonitorPrinters monitors all printers for print status changes
func (b *FilamentBridge) MonitorPrinters() {
	// Get a safe snapshot of the config to prevent iteration issues
	configSnapshot := b.GetConfigSnapshot()
	if configSnapshot == nil || len(configSnapshot.Printers) == 0 {
		return
	}

	// Monitor each printer using PrusaLink
	for printerID, printerConfig := range configSnapshot.Printers {
		if printerID == "no_printers" {
			continue // Skip placeholder
		}
		go func(printerID string, config PrinterConfig) {
			if err := b.monitorPrusaLink(printerID, config); err != nil {
				log.Printf("Error monitoring printer %s (%s): %v", config.IPAddress, printerID, err)
			}
		}(printerID, printerConfig)
	}
}

// activeJob is the persisted in-flight print for a single printer.
type activeJob struct {
	PrinterID    string
	JobID        int
	Filename     string
	LastProgress float64         // highest progress fraction (0..1) seen while printing
	StartedAt    time.Time       // when the job was first seen printing
	Usage        map[int]float64 // full slicer filament estimate (g) per toolhead, from file.meta
}

// getActiveJob returns the persisted in-flight job for a printer, or nil if none.
func (b *FilamentBridge) getActiveJob(printerID string) (*activeJob, error) {
	var (
		aj        activeJob
		usageJSON string
	)
	err := b.db.QueryRow(
		`SELECT printer_id, job_id, filename, last_progress, started_at, usage_json FROM active_jobs WHERE printer_id = ?`,
		printerID,
	).Scan(&aj.PrinterID, &aj.JobID, &aj.Filename, &aj.LastProgress, &aj.StartedAt, &usageJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if usageJSON != "" {
		if err := json.Unmarshal([]byte(usageJSON), &aj.Usage); err != nil {
			log.Printf("Warning: failed to decode stored usage for %s: %v", printerID, err)
		}
	}
	return &aj, nil
}

// upsertActiveJob writes (creates or replaces) the in-flight job for a printer.
func (b *FilamentBridge) upsertActiveJob(aj *activeJob) error {
	usageJSON := ""
	if len(aj.Usage) > 0 {
		if data, err := json.Marshal(aj.Usage); err == nil {
			usageJSON = string(data)
		}
	}
	_, err := b.db.Exec(
		`INSERT INTO active_jobs (printer_id, job_id, filename, last_progress, started_at, usage_json, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(printer_id) DO UPDATE SET
		     job_id=excluded.job_id, filename=excluded.filename, last_progress=excluded.last_progress,
		     started_at=excluded.started_at, usage_json=excluded.usage_json, updated_at=excluded.updated_at`,
		aj.PrinterID, aj.JobID, aj.Filename, aj.LastProgress, aj.StartedAt, usageJSON, time.Now(),
	)
	return err
}

// clearActiveJob removes the in-flight job record for a printer.
func (b *FilamentBridge) clearActiveJob(printerID string) error {
	b.clearScanAttempts(printerID)
	b.clearRunoutState(printerID)
	_, err := b.db.Exec(`DELETE FROM active_jobs WHERE printer_id = ?`, printerID)
	return err
}

// isJobRecorded reports whether a (printer, job) pair has already had its
// filament usage recorded. Jobs without a real id (jobID == 0) can't be
// deduped and are treated as unrecorded.
func (b *FilamentBridge) isJobRecorded(printerID string, jobID int) (bool, error) {
	if jobID == 0 {
		return false, nil
	}
	var one int
	err := b.db.QueryRow(
		`SELECT 1 FROM recorded_jobs WHERE printer_id = ? AND job_id = ?`, printerID, jobID,
	).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// markJobRecorded marks a (printer, job) pair as recorded so it is never counted again.
func (b *FilamentBridge) markJobRecorded(printerID string, jobID int, filename string, scale float64) error {
	if jobID == 0 {
		return nil // no stable id to dedupe on; nothing to record
	}
	_, err := b.db.Exec(
		`INSERT OR IGNORE INTO recorded_jobs (printer_id, job_id, filename, scale, recorded_at) VALUES (?, ?, ?, ?, ?)`,
		printerID, jobID, filename, scale, time.Now(),
	)
	return err
}

// noteConnectivity performs edge-triggered logging of a printer's reachability.
// It logs once when a printer first becomes unreachable and once when it later
// recovers, suppressing the repeated warnings that an offline printer would
// otherwise emit on every poll cycle and every dashboard/API request. err is the
// result of the reachability check (nil means reachable). State is shared across
// all callers (monitor loop, broadcasts, web handlers) so the offline/online
// transition is logged exactly once regardless of which path observes it.
func (b *FilamentBridge) noteConnectivity(printerID, ipAddress, name string, err error) {
	b.offlineMutex.Lock()
	defer b.offlineMutex.Unlock()

	wasOffline := b.offlinePrinters[printerID]
	if err != nil {
		if !wasOffline {
			log.Printf("Printer %s (%s - %s) is offline, suppressing further offline warnings until it returns: %v",
				ipAddress, printerID, name, err)
			b.offlinePrinters[printerID] = true
		}
		return
	}
	if wasOffline {
		log.Printf("Printer %s (%s - %s) is back online", ipAddress, printerID, name)
		delete(b.offlinePrinters, printerID)
	}
}

// noteStateChange performs edge-triggered logging of a printer's state: one log
// line per transition (IDLE -> PRINTING, PRINTING -> FINISHED, ...) instead of
// one per poll cycle.
func (b *FilamentBridge) noteStateChange(printerID, name, state, jobName string) {
	b.offlineMutex.Lock()
	defer b.offlineMutex.Unlock()

	previous, seen := b.printerStates[printerID]
	if state == previous {
		return
	}
	b.printerStates[printerID] = state

	jobSuffix := ""
	if jobName != "" && jobName != "No active job" {
		jobSuffix = fmt.Sprintf(" (job: %s)", jobName)
	}
	if !seen {
		log.Printf("Printer %s: state %s%s", name, state, jobSuffix)
		return
	}
	log.Printf("Printer %s: %s -> %s%s", name, previous, state, jobSuffix)
}

// maxEstimateScanAttempts bounds how often the header scan is retried for per
// print file, in case the printer refuses or fails while printing.
// Print-end fallback still runs regardless,
const maxEstimateScanAttempts = 3

// shouldScanForEstimate reports whether a header scan should run now for this
// printer+file combo. If true the caller owns the in-flight slot and must
// release it with finishScanForEstimate.
func (b *FilamentBridge) shouldScanForEstimate(printerID, filename string) bool {
	b.scanMutex.Lock()
	defer b.scanMutex.Unlock()

	if b.scanInFlight[printerID] {
		return false
	}
	key := printerID + "|" + filename
	if b.scanAttempts[key] >= maxEstimateScanAttempts {
		return false
	}
	b.scanAttempts[key]++
	b.scanInFlight[printerID] = true
	return true
}

// finishScanForEstimate releases the in-flight scan slot for a printer.
func (b *FilamentBridge) finishScanForEstimate(printerID string) {
	b.scanMutex.Lock()
	defer b.scanMutex.Unlock()
	delete(b.scanInFlight, printerID)
}

// clearScanAttempts forgets header-scan attempt counts for a printer, so the
// next job starts with a fresh budget.
func (b *FilamentBridge) clearScanAttempts(printerID string) {
	b.scanMutex.Lock()
	defer b.scanMutex.Unlock()
	prefix := printerID + "|"
	for key := range b.scanAttempts {
		if strings.HasPrefix(key, prefix) {
			delete(b.scanAttempts, key)
		}
	}
}

// monitorPrusaLink monitors a single printer using PrusaLink API
func (b *FilamentBridge) monitorPrusaLink(printerID string, config PrinterConfig) error {
	// Read timeouts from a snapshot: b.config can be swapped by ReloadConfig at
	// any time, so direct field reads from this goroutine would race.
	cfg := b.GetConfigSnapshot()
	if cfg == nil {
		return nil
	}
	client := NewPrusaLinkClient(config.IPAddress, config.APIKey, cfg.PrusaLinkTimeout, cfg.PrusaLinkFileDownloadTimeout)

	status, err := client.GetStatus()
	if err != nil {
		b.noteConnectivity(printerID, config.IPAddress, config.Name, err)
		return nil // Don't fail the entire monitoring cycle for one printer
	}
	b.noteConnectivity(printerID, config.IPAddress, config.Name, nil)

	jobInfo, err := client.GetJobInfo()
	if err != nil {
		log.Printf("Warning: Failed to get job info from %s (%s): %v", config.IPAddress, printerID, err)
		// Continue with status-only monitoring if job info fails
		jobInfo = &PrusaLinkJob{}
	}

	currentState := status.Printer.State
	jobName := "No active job"
	currentJobFilename := ""
	if jobInfo.File.Name != "" {
		jobName = jobInfo.File.DisplayName // Use display name for better readability
		// Use the download path directly from refs - it's already in the correct format
		if jobInfo.File.Refs.Download != "" {
			currentJobFilename = strings.TrimPrefix(jobInfo.File.Refs.Download, "/")
		} else {
			// Fallback: construct the path manually
			storage := strings.TrimPrefix(jobInfo.File.Path, "/")
			currentJobFilename = storage + "/" + jobInfo.File.Name
		}
	}

	// Load the persisted in-flight job (source of truth; survives restarts).
	active, err := b.getActiveJob(printerID)
	if err != nil {
		log.Printf("Warning: failed to read active job for %s: %v", printerID, err)
	}

	isPrinting := currentState == StatePrinting || currentState == StatePaused ||
		currentState == StateAttention
	// A job has ended if the printer is now in any terminal state: a clean finish
	// (FINISHED/IDLE) or a cancel/failure (STOPPED/ERROR). Catching STOPPED/ERROR
	// here is what lets us record a cancelled print's actual usage.
	isTerminal := currentState == StateIdle || currentState == StateFinished ||
		currentState == StateStopped || currentState == StateError

	b.noteStateChange(printerID, config.Name, currentState, jobName)

	switch {
	case isPrinting:
		// Build/refresh the persisted in-flight record.
		aj := &activeJob{PrinterID: printerID, StartedAt: time.Now()}
		// Treat this as a continuation of the tracked job when paused, when the job
		// id matches, or when the printer doesn't report an id at all.
		if active != nil && (currentState == StatePaused || currentState == StateAttention || active.JobID == jobInfo.ID || jobInfo.ID == 0) {
			aj.JobID = active.JobID
			aj.Filename = active.Filename
			aj.StartedAt = active.StartedAt
			aj.LastProgress = active.LastProgress
			aj.Usage = active.Usage
		}
		if jobInfo.ID != 0 {
			aj.JobID = jobInfo.ID
		}
		if currentJobFilename != "" {
			aj.Filename = currentJobFilename
		}
		// Track monotonic max progress so a late/stale low reading can't shrink it.
		if currentState == StatePrinting {
			if p := normalizeProgress(jobInfo.Progress); p > aj.LastProgress {
				aj.LastProgress = p
			}
		}
		// Capture the slicer filament estimate while the job is running, so that
		// completion and cancellation can record usage without any fetch at
		// print end. Two sources, cheapest first:
		//   Job metadata from the API - free, but some firmware (like the
		//   CORE One) never serves it.
		//   OR Streaming scan of the print file's header.  .bgcode keeps the
		//      estimate in its first few KB, so this reads a tiny slice of the
		//      file. Attempt-limited per job in case the printer refuses file
		//      reads while printing.
		if len(aj.Usage) == 0 {
			if usage := filamentUsageFromMeta(jobInfo.File.Meta); len(usage) > 0 {
				aj.Usage = usage
			} else if aj.Filename != "" && b.shouldScanForEstimate(printerID, aj.Filename) {
				if usage, err := client.ScanGcodeFilamentUsage(aj.Filename, cfg.PrusaLinkFileDownloadTimeout); err != nil {
					log.Printf("Warning: could not scan %s for filament estimate (will retry): %v", aj.Filename, err)
				} else if len(usage) > 0 {
					aj.Usage = usage
					log.Printf("Captured filament estimate for %s from file header: %v", config.Name, usage)
				}
				b.finishScanForEstimate(printerID)
			}
		}
		if aj.Filename != "" {
			if err := b.upsertActiveJob(aj); err != nil {
				log.Printf("Warning: failed to persist active job for %s: %v", printerID, err)
			}
		}

		// With the estimate in hand, warn (and optionally pause) if the mapped
		// spool has less filament remaining than the print still needs.
		b.checkRunoutWarnings(printerID, config, client, aj)

	case isTerminal && active != nil && active.Filename != "":
		// A tracked job has ended. Classify how it ended to decide how much usage to record:
		//   FINISHED        -> completed, full estimate
		//   STOPPED / ERROR -> cancelled/failed, estimate * last-seen progress
		//   IDLE            -> ambiguous (some firmware skips FINISHED): completed only
		//                      if we last saw it near 100%, otherwise partial
		completed := currentState == StateFinished ||
			(currentState == StateIdle && active.LastProgress >= PrintCompletionProgressThreshold)
		usageScale := 1.0
		if !completed {
			usageScale = active.LastProgress
		}

		// Idempotency: never record the same job twice (survives restart/double-detect).
		if recorded, err := b.isJobRecorded(printerID, active.JobID); err != nil {
			log.Printf("Warning: failed to check recorded-jobs ledger for %s job %d: %v", printerID, active.JobID, err)
		} else if recorded {
			log.Printf("Job %d on %s already recorded; clearing tracking", active.JobID, printerID)
			b.clearActiveJob(printerID)
			return nil
		}

		// Guard against two overlapping monitor cycles recording the same printer's usage.
		b.mutex.Lock()
		if b.processingPrints[printerID] {
			b.mutex.Unlock()
			return nil
		}
		b.processingPrints[printerID] = true
		b.mutex.Unlock()
		defer func() {
			b.mutex.Lock()
			b.processingPrints[printerID] = false
			b.mutex.Unlock()
		}()

		if completed {
			log.Printf("Print finished for %s (%s): %s (state: %s, file: %s)",
				config.IPAddress, printerID, jobName, currentState, active.Filename)
		} else {
			log.Printf("Print cancelled/failed for %s (%s): %s (state: %s, ~%.0f%% printed, file: %s)",
				config.IPAddress, printerID, jobName, currentState, usageScale*100, active.Filename)
		}

		if err := b.handlePrintEnded(config, client, active, usageScale, completed, cfg.PrusaLinkFileDownloadTimeout); err != nil {
			// Whole-job failure (download/parse/no-data) before any spool was written.
			// A print error was already logged; clear tracking so we don't reprocess
			// it every poll while the printer sits idle.
			log.Printf("Error handling print end for %s: %v", printerID, err)
			b.clearActiveJob(printerID)
			return nil
		}

		// Success: record in the idempotency ledger, then clear tracking.
		if err := b.markJobRecorded(printerID, active.JobID, active.Filename, usageScale); err != nil {
			log.Printf("Warning: failed to mark job %d as recorded for %s: %v", active.JobID, printerID, err)
		}
		b.clearActiveJob(printerID)

	case isTerminal && active != nil && active.Filename == "":
		// Stale tracking row with no filename - drop it.
		b.clearActiveJob(printerID)
	}

	return nil
}

// normalizeProgress converts a PrusaLink progress value to a fraction in [0,1].
// The PrusaLink v1 API reports job progress as a percentage (0..100), so the
// value is always divided by 100. Do not try to guess whether a value <= 1.0
// is "already a fraction": that reads a print cancelled at 1% (progress=1.0)
// as 100% complete and records up to 100x the actual usage.
func normalizeProgress(p float64) float64 {
	p = p / 100.0
	if p < 0 {
		return 0
	}
	if p > 1.0 {
		return 1.0
	}
	return p
}

// handlePrintEnded records filament usage for a print that has ended. usageScale
// is the fraction (0..1) of the slicer's estimate to record: 1.0 for a completed print, or
// the last-seen progress for one cancelled/failed partway through; completed
// records how the print ended in the history. It prefers the filament estimate
// captured from job metadata while printing, and only downloads and parses the
// G-code file as a fallback when that metadata was unavailable.
func (b *FilamentBridge) handlePrintEnded(config PrinterConfig, prusaClient *PrusaLinkClient, active *activeJob, usageScale float64, completed bool, fileDownloadTimeout int) error {
	printerName := resolvePrinterName(config)
	filename := active.Filename

	log.Printf("Print ended on %s: %s (recording %.0f%% of estimate)", printerName, filename, usageScale*100)

	if filename == "" {
		errorMsg := "no filename available for print processing"
		b.addPrintError(printerName, "unknown", errorMsg)
		return fmt.Errorf("%s", errorMsg)
	}
	if usageScale < 0 {
		usageScale = 0
	}

	// Determine the full per-toolhead slicer estimate. Prefer metadata captured
	// while printing; fall back to downloading and parsing the G-code file.
	usage := active.Usage
	source := "job metadata"
	if len(usage) == 0 {
		// Nothing printed and no metadata: skip the expensive download, record nothing.
		if usageScale == 0 {
			log.Printf("Print %s on %s ended at ~0%% with no metadata; nothing to record", filename, printerName)
			return nil
		}

		// Prefer a streaming header scan: .bgcode keeps the estimate in its
		// first few KB, so this reads a tiny slice of the file where a full
		// download of a large file cannot finish inside any timeout at
		// PrusaLink's notoriously low transfer speed.
		log.Printf("No stored estimate for %s; scanning file header for filament usage: %s", printerName, filename)
		scanned, scanErr := prusaClient.ScanGcodeFilamentUsage(filename, fileDownloadTimeout)
		if scanErr == nil && len(scanned) > 0 {
			usage = scanned
			source = "file header scan"
		} else {
			if scanErr != nil {
				log.Printf("Warning: header scan failed for %s (%v); falling back to full download", filename, scanErr)
			}
			gcodeContent, err := prusaClient.GetGcodeFileWithRetry(filename, fileDownloadTimeout)
			if err != nil {
				errorMsg := fmt.Sprintf("failed to download G-code file after retries: %v", err)
				b.addPrintError(printerName, filename, errorMsg)
				return fmt.Errorf("%s", errorMsg)
			}
			parsed, err := prusaClient.ParseGcodeFilamentUsage(gcodeContent)
			if err != nil {
				errorMsg := fmt.Sprintf("failed to parse G-code for filament usage: %v", err)
				b.addPrintError(printerName, filename, errorMsg)
				return fmt.Errorf("%s", errorMsg)
			}
			usage = parsed
			source = "G-code file"
		}
	}

	if len(usage) == 0 {
		errorMsg := "no filament usage data found (metadata or G-code)"
		b.addPrintError(printerName, filename, errorMsg)
		return fmt.Errorf("%s", errorMsg)
	}

	// For a cancelled/failed print, scale the full slicer estimate down to the
	// fraction that was actually printed. PrusaLink progress is time-based, not
	// extrusion-based, so this is an approximation - but far closer than 0% or 100%.
	if usageScale < 1.0 {
		scaled := make(map[int]float64, len(usage))
		for toolheadID, weight := range usage {
			scaled[toolheadID] = weight * usageScale
		}
		usage = scaled
		log.Printf("Scaled filament usage to ~%.0f%% for partial print: %s", usageScale*100, filename)
	}

	log.Printf("Filament usage for %s (source: %s): %+v", filename, source, usage)

	printStarted := active.StartedAt
	if printStarted.IsZero() {
		printStarted = time.Now()
	}

	status := "completed"
	if !completed {
		status = "cancelled"
	}

	// Process filament usage using helper function
	if err := b.processFilamentUsage(printerName, usage, filename, printStarted, status); err != nil {
		log.Printf("Error processing filament usage: %v", err)
		return err
	}

	return nil
}

// GetPrintErrors returns all unacknowledged print errors
func (b *FilamentBridge) GetPrintErrors() []PrintError {
	b.errorMutex.RLock()
	defer b.errorMutex.RUnlock()

	var errors []PrintError
	for _, err := range b.printErrors {
		if !err.Acknowledged {
			errors = append(errors, err)
		}
	}
	return errors
}

// AcknowledgePrintError marks a print error as acknowledged
func (b *FilamentBridge) AcknowledgePrintError(errorID string) error {
	b.errorMutex.Lock()
	defer b.errorMutex.Unlock()

	if err, exists := b.printErrors[errorID]; exists {
		err.Acknowledged = true
		b.printErrors[errorID] = err
		return nil
	}
	return fmt.Errorf("print error not found: %s", errorID)
}

// sanitizeErrorID replaces problematic characters in error IDs to make them URL-safe
func sanitizeErrorID(s string) string {
	// Replace forward slashes with underscores
	s = strings.ReplaceAll(s, "/", "_")
	// Replace spaces with underscores
	s = strings.ReplaceAll(s, " ", "_")
	// Replace backslashes with underscores
	s = strings.ReplaceAll(s, "\\", "_")
	return s
}

// addPrintError adds a new print error
func (b *FilamentBridge) addPrintError(printerName, filename, errorMsg string) {
	b.errorMutex.Lock()
	defer b.errorMutex.Unlock()

	// Sanitize printer name and filename to ensure URL-safe error IDs
	sanitizedPrinterName := sanitizeErrorID(printerName)
	sanitizedFilename := sanitizeErrorID(filename)
	errorID := fmt.Sprintf("%s_%s_%d", sanitizedPrinterName, sanitizedFilename, time.Now().Unix())
	b.printErrors[errorID] = PrintError{
		ID:           errorID,
		PrinterName:  printerName,
		Filename:     filename,
		Error:        errorMsg,
		Timestamp:    time.Now(),
		Acknowledged: false,
	}

	log.Printf("Print processing failed for %s (%s): %s - Manual Spoolman update required",
		printerName, filename, errorMsg)
}

// GetStatus gets current status of all printers and mappings
func (b *FilamentBridge) GetStatus() (*PrinterStatus, error) {
	status := &PrinterStatus{
		Printers:         make(map[string]PrinterData),
		ToolheadMappings: make(map[string]map[int]ToolheadMapping),
		Timestamp:        time.Now(),
	}

	// Get a safe snapshot of the config to prevent iteration issues
	configSnapshot := b.GetConfigSnapshot()
	if configSnapshot == nil {
		// No printers configured
		status.Printers["no_printers"] = PrinterData{
			Name:  "No Printers Configured",
			State: StateNotConfigured,
		}
		return status, nil
	}

	// Get printer statuses from PrusaLink
	if len(configSnapshot.Printers) > 0 {
		for printerID, printerConfig := range configSnapshot.Printers {
			if printerID == "no_printers" {
				continue // Skip placeholder
			}

			client := NewPrusaLinkClient(printerConfig.IPAddress, printerConfig.APIKey, configSnapshot.PrusaLinkTimeout, configSnapshot.PrusaLinkFileDownloadTimeout)

			// Use the configured printer name, not the hostname from PrusaLink
			printerName := printerConfig.Name

			// Get current status
			printerStatus, err := client.GetStatus()
			if err != nil {
				// Edge-triggered: logs once on the transition to offline and once
				// on recovery, rather than on every broadcast/web request.
				b.noteConnectivity(printerID, printerConfig.IPAddress, printerName, err)
				status.Printers[printerID] = PrinterData{
					Name:  printerName,
					State: StateOffline,
				}
				continue
			}
			b.noteConnectivity(printerID, printerConfig.IPAddress, printerName, nil)

			status.Printers[printerID] = PrinterData{
				Name:  printerName,
				State: printerStatus.Printer.State,
			}
		}
	} else {
		// No printers configured
		status.Printers["no_printers"] = PrinterData{
			Name:  "No Printers Configured",
			State: StateNotConfigured,
		}
	}

	// Get toolhead mappings for all printers
	for printerID, printerConfig := range configSnapshot.Printers {
		if printerID == "no_printers" {
			continue // Skip placeholder
		}

		printerName := printerConfig.Name
		mappings, err := b.GetToolheadMappings(printerName)
		if err != nil {
			log.Printf("Error getting toolhead mappings for %s: %v", printerName, err)
			mappings = make(map[int]ToolheadMapping)
		}

		// Get toolhead names for this printer
		toolheadNames, err := b.GetAllToolheadNames(printerID)
		if err != nil {
			log.Printf("Warning: Failed to get toolhead names for printer %s: %v", printerID, err)
			toolheadNames = make(map[int]string)
		}

		// Create enhanced mappings for ALL toolheads (including unmapped ones)
		enhancedMappings := make(map[int]ToolheadMapping)
		for toolheadID := 0; toolheadID < printerConfig.Toolheads; toolheadID++ {
			// Get display name (custom or default)
			var displayName string
			if name, exists := toolheadNames[toolheadID]; exists {
				displayName = name
			} else {
				displayName = fmt.Sprintf("Toolhead %d", toolheadID)
			}

			// If this toolhead has a mapping, use it and add display name
			if mapping, exists := mappings[toolheadID]; exists {
				mapping.DisplayName = displayName
				enhancedMappings[toolheadID] = mapping
			} else {
				// Create empty mapping with just display name for unmapped toolheads
				enhancedMappings[toolheadID] = ToolheadMapping{
					PrinterName: printerName,
					ToolheadID:  toolheadID,
					SpoolID:     0, // No spool mapped
					DisplayName: displayName,
				}
			}
		}
		status.ToolheadMappings[printerID] = enhancedMappings
	}

	return status, nil
}

// processFilamentUsage processes filament usage updates for all toolheads.
// printStarted and status are recorded in the print history for each toolhead
// entry, unless local print history is disabled in settings.
func (b *FilamentBridge) processFilamentUsage(printerName string, filamentUsage map[int]float64, jobName string, printStarted time.Time, status string) error {
	historyEnabled, err := b.GetPrintHistoryEnabled()
	if err != nil {
		log.Printf("Warning: failed to read print history setting, keeping history: %v", err)
		historyEnabled = true
	}

	// Update Spoolman with filament usage for each toolhead
	for toolheadID, usedWeight := range filamentUsage {
		if usedWeight <= 0 {
			continue
		}

		// Get the mapped spool for this toolhead
		spoolID, err := b.GetToolheadMapping(printerName, toolheadID)
		if err != nil {
			log.Printf("Error getting toolhead mapping for %s toolhead %d: %v",
				printerName, toolheadID, err)
			continue
		}

		if spoolID == 0 {
			log.Printf("No spool mapped to %s toolhead %d, skipping filament usage update",
				printerName, toolheadID)
			continue
		}

		// Update Spoolman
		if err := b.spoolman.UpdateSpoolUsage(spoolID, usedWeight); err != nil {
			log.Printf("Error updating spool %d usage: %v", spoolID, err)
			continue
		}

		// Log the usage in our database (unless local history is disabled)
		if historyEnabled {
			if err := b.LogPrintUsage(printerName, toolheadID, spoolID, usedWeight, jobName, printStarted, status); err != nil {
				log.Printf("Error logging print usage: %v", err)
			}
		}

		log.Printf("Updated spool %d: used %.2fg filament on %s toolhead %d",
			spoolID, usedWeight, printerName, toolheadID)
	}

	// Summary log
	if len(filamentUsage) > 0 {
		log.Printf("Print completion processing finished for %s: processed %d toolheads", printerName, len(filamentUsage))
	} else {
		log.Printf("No filament usage data processed for %s", printerName)
	}

	return nil
}

// Close closes the database connection
func (b *FilamentBridge) Close() error {
	if b.db != nil {
		return b.db.Close()
	}
	return nil
}
