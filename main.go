package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version is the release version, injected at build time via
// -ldflags "-X main.version=v1.2.3" (set from the git tag by CI).
var version = "dev"

func main() {
	// Command line flags
	var (
		webOnly     = flag.Bool("web-only", false, "Run only the web interface")
		bridgeOnly  = flag.Bool("bridge-only", false, "Run only the bridge service")
		port        = flag.String("port", DefaultWebPort, "Web interface port")
		host        = flag.String("host", "0.0.0.0", "Web interface host")
		showVersion = flag.Bool("version", false, "Print version and exit")

		// Developer probe for Bambu MQTT (experimental). Connects, dumps raw
		// reports, then exits. Does not touch the database or web server.
		bambuProbe  = flag.Bool("bambu-probe", false, "Developer: connect to a Bambu printer over MQTT, dump raw reports + FTPS slice_info, then exit")
		bambuWatch  = flag.Bool("bambu-watch", false, "Developer: watch a Bambu printer's live state until a print ends, then print the grams that would be recorded")
		bambuWatchS = flag.Int("bambu-watch-seconds", 1800, "Max seconds to watch (with -bambu-watch)")
		bambuIP     = flag.String("bambu-ip", "", "Bambu printer IP (with -bambu-probe/-bambu-watch)")
		bambuSerial = flag.String("bambu-serial", "", "Bambu printer serial number (with -bambu-probe/-bambu-watch)")
		bambuCode   = flag.String("bambu-code", "", "Bambu LAN access code (with -bambu-probe/-bambu-watch)")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("FilaBridge %s\n", version)
		return
	}

	if *bambuProbe || *bambuWatch {
		installLogSplitter()
		if *bambuIP == "" || *bambuSerial == "" || *bambuCode == "" {
			log.Fatal("bambu-probe/-bambu-watch require -bambu-ip, -bambu-serial, and -bambu-code")
		}
		if *bambuWatch {
			runBambuWatch(*bambuIP, *bambuSerial, *bambuCode, time.Duration(*bambuWatchS)*time.Second)
		} else {
			runBambuProbe(*bambuIP, *bambuSerial, *bambuCode, 60*time.Second)
		}
		return
	}

	// Route informational log output to stdout and warnings/errors to stderr
	// (the log package otherwise sends everything to stderr).
	installLogSplitter()

	log.Printf("FilaBridge %s starting", version)

	// Create bridge instance first (with default config)
	bridge, err := NewFilamentBridge(nil)
	if err != nil {
		log.Fatalf("Failed to create bridge: %v", err)
	}
	defer bridge.Close()

	// Load configuration from database
	config, err := LoadConfig(bridge)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Update bridge with loaded config
	if err := bridge.UpdateConfig(config); err != nil {
		log.Fatalf("Failed to update bridge config: %v", err)
	}

	// Override port from config if not specified
	if *port == DefaultWebPort && config.WebPort != DefaultWebPort {
		*port = config.WebPort
	}

	// Handle graceful shutdown. A single goroutine receives the signal and
	// closes done; a close wakes every waiter, whereas receiving directly from
	// sigChan in multiple goroutines would deliver the signal to only one of
	// them (and possibly not the one blocking main).
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		<-sigChan
		close(done)
	}()

	// Start NFC session cleanup background task
	go func() {
		ticker := time.NewTicker(1 * time.Minute) // Clean up every minute
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := bridge.cleanupExpiredSessions(); err != nil {
					log.Printf("Error cleaning up NFC sessions: %v", err)
				}
			case <-done:
				return
			}
		}
	}()

	if *webOnly {
		// Run only web interface
		fmt.Println("Starting web interface only...")
		webServer := NewWebServer(bridge)
		go func() {
			if err := webServer.Start(*port); err != nil {
				log.Fatalf("Web server error: %v", err)
			}
		}()

		// Wait for shutdown signal
		<-done
		fmt.Println("Shutting down web server...")

	} else if *bridgeOnly {
		// Run only bridge service
		fmt.Println("Starting bridge service only...")
		fmt.Printf("Monitoring printers: %v\n", getPrinterNames(config))
		fmt.Printf("Spoolman URL: %s\n", config.SpoolmanURL)
		fmt.Printf("Poll interval: %v\n", config.PollInterval)

		// Start monitoring in a goroutine
		go func() {
			ticker := time.NewTicker(config.PollInterval)
			defer ticker.Stop()

			// Run initial check
			bridge.MonitorPrinters()

			// Continue monitoring
			for {
				select {
				case <-ticker.C:
					bridge.MonitorPrinters()
				case <-done:
					return
				}
			}
		}()

		// Wait for shutdown signal
		<-done
		fmt.Println("Shutting down bridge service...")

	} else {
		// Run both bridge service and web interface
		fmt.Println("Starting both bridge service and web interface...")
		fmt.Printf("Monitoring printers: %v\n", getPrinterNames(config))
		fmt.Printf("Spoolman URL: %s\n", config.SpoolmanURL)
		fmt.Printf("Poll interval: %v\n", config.PollInterval)
		fmt.Printf("Web interface: http://%s:%s\n", *host, *port)

		// Create web server first so we can pass it to monitoring
		webServer := NewWebServer(bridge)

		// Start bridge monitoring in a goroutine
		go func() {
			ticker := time.NewTicker(config.PollInterval)
			defer ticker.Stop()

			// Run initial check
			bridge.MonitorPrinters()
			// Broadcast initial status
			webServer.BroadcastStatus()

			// Continue monitoring
			for {
				select {
				case <-ticker.C:
					bridge.MonitorPrinters()
					// Broadcast status after each monitoring cycle
					webServer.BroadcastStatus()
				case <-done:
					return
				}
			}
		}()

		// Start web server in a goroutine
		go func() {
			if err := webServer.Start(*port); err != nil {
				log.Fatalf("Web server error: %v", err)
			}
		}()

		// Wait for shutdown signal
		<-done
		fmt.Println("Shutting down services...")
	}
}

// getPrinterNames returns a slice of printer names from config
func getPrinterNames(config *Config) []string {
	names := make([]string, 0, len(config.Printers))
	for name := range config.Printers {
		names = append(names, name)
	}
	return names
}
