package main

// Printer states
const (
	StateIdle          = "IDLE"
	StatePrinting      = "PRINTING"
	StatePaused        = "PAUSED"
	StateFinished      = "FINISHED"
	StateStopped       = "STOPPED"   // PrusaLink reports a user-cancelled print as STOPPED
	StateError         = "ERROR"     // PrusaLink reports a failed/aborted print as ERROR
	StateAttention     = "ATTENTION" // Printer needs user action mid-job (filament runout, crash detection); the job is still in-flight
	StateOffline       = "offline"
	StateNotConfigured = "not_configured"
)

// PrintCompletionProgressThreshold is the minimum last-seen progress fraction
// (0..1) for a print that ends in a bare IDLE state (no explicit FINISHED) to be
// treated as completed rather than cancelled. Below this, the print's usage is
// recorded proportionally to how far it got.
const PrintCompletionProgressThreshold = 0.95

// Default configuration values
const (
	DefaultSpoolmanURL  = "http://localhost:7912"
	DefaultWebPort      = "5000"
	DefaultPollInterval = 30
	DefaultDBFileName   = "filabridge.db"
)

// Database configuration keys
const (
	ConfigKeySpoolmanURL                     = "spoolman_url"
	ConfigKeyPollInterval                    = "poll_interval"
	ConfigKeyWebPort                         = "web_port"
	ConfigKeyPrusaLinkTimeout                = "prusalink_timeout"
	ConfigKeyPrusaLinkFileDownloadTimeout    = "prusalink_file_download_timeout"
	ConfigKeySpoolmanTimeout                 = "spoolman_timeout"
	ConfigKeySpoolmanUsername                = "spoolman_username"
	ConfigKeySpoolmanPassword                = "spoolman_password"
	ConfigKeyAutoAssignPreviousSpoolEnabled  = "auto_assign_previous_spool_enabled"
	ConfigKeyAutoAssignPreviousSpoolLocation = "auto_assign_previous_spool_location"
	ConfigKeyPrintHistoryEnabled             = "print_history_enabled"
	ConfigKeyRunoutWarningEnabled            = "runout_warning_enabled"
	ConfigKeyRunoutPauseEnabled              = "runout_pause_enabled"
)

// HTTP timeouts
const (
	PrusaLinkTimeout             = 10  // seconds
	PrusaLinkFileDownloadTimeout = 300 // seconds for file downloads (USB storage can be slow)
	SpoolmanTimeout              = 10  // seconds
)
