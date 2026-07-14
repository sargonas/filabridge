# FilaBridge

[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](https://www.gnu.org/licenses/gpl-3.0)
[![Go Version](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go)](https://golang.org/)
[![GitHub release](https://img.shields.io/github/v/release/sargonas/filabridge)](https://github.com/sargonas/filabridge/releases)

A self-hosted Go microservice that bridges PrusaLink-compatible printers and Spoolman for (mostly) automatic filament inventory management. Originally designed for Prusa printers (CORE One, XL, MK4, etc.) but will work with any printer that supports the PrusaLink API.

FilaBridge was created by [needo37](https://github.com/needo37/filabridge), who has since archived the original project. This repository continues its development and maintenance. Many thanks to needo37 for building FilaBridge and releasing it under the GPL so it could live on.

### The Problem

Spoolman is an excellent tool to track one's filament inventory. However, manually having to update filament usage after every print is prone to human error and mistakes. For owners of a Prusa printer, there are ways to automate this thanks to PrusaLink, but you need a layer between the Printer and Spoolman to do so. [needo37](https://github.com/needo37) created just such a tool before moving on to other projects, and this is a continuation of that work.

## Features

- **PrusaLink Compatibility**: Works with any PrusaLink-compatible printer (Prusa CORE One, XL, MK4, Mini, and more)
- **Real-time Dashboard**: Web interface with live updates via WebSocket connections
- **Multi-Toolhead Support**: Seamlessly handles single and multi-toolhead printers (tested with 5-toolhead Prusa XL, looking for INDX testers!)
- **Smart Usage Tracking**: Reads the slicer's per-toolhead filament estimates from job metadata (parsing the G-code file as a fallback) to track consumption per toolhead
- **Cancelled Print Tracking**: Prints that are cancelled or fail partway still have their filament usage recorded, scaled to how far they got
- **Reliable Recording**: In-flight prints are tracked in the database, so they survive restarts and are never counted twice
- **Print History**: Dashboard tab showing recent prints with completion status, spool used, filament recorded, and run time
- **Persistent Storage**: SQLite database stores toolhead mappings and complete print history
- **High Performance**: Single lightweight binary with a single DB file, minimal resource usage, fast execution
- **Web-based Config**: No config files needed - manage everything through the web UI
- **Smart Spool Search**: Search and filter spools by ID, material, brand, or name with real-time filtering
- **Error Handling**: Print error detection with acknowledgment system for failed filament tracking
- **Auto-mapping**: Automatic spool assignment when selecting from dropdown menus
- **NFC Tag Support**: Generate QR codes and program NFC tags for spools, filaments, and locations
- **Smart Scanning**: Two-step NFC workflow - scan spool + location for instant assignment
- **Quick-Assign Tags**: Single-printer setups get one-scan tags that assign a spool straight to the printer, no location tag needed
- **Location Tracking**: Track spools in custom locations (dryboxes) or printer toolheads
- **Smart Housekeeping**: If a new spool is "loaded" to a printer, the previous will be returned to a pre-set default location

## Why FilaBridge?

Managing filament inventory across multiple 3D printers, or even one, can be tedious. FilaBridge automates this by:
- Monitoring your printers in real-time with live WebSocket updates
- Tracking which spools are loaded on which toolheads
- Automatically updating your Spoolman inventory when prints complete
- Providing accurate filament usage from the slicer's own per-toolhead estimates
- Handling errors gracefully with clear notifications and acknowledgment system
- Using NFC tags or QR codes to quickly assign spools to printers or storage locations
- Tracking filament locations across your workshop

No more manual updates or guesswork about remaining filament!

## Screenshots

![FilaBridge Dashboard](https://github.com/sargonas/filabridge/blob/main/screenshots/dashboard.png?raw=true)
*FilaBridge main dashboard showing printer status and toolhead mappings*

![Spool Tags Management](https://github.com/sargonas/filabridge/blob/main/screenshots/spool_tags.png?raw=true)
*NFC Management interface for generating QR codes for individual spools*

![Filament Tags Management](https://github.com/sargonas/filabridge/blob/main/screenshots/filament_tags.png?raw=true)
*Filament type QR code generation for new unopened spools*

![Location Tags Management](https://github.com/sargonas/filabridge/blob/main/screenshots/location_tags.png?raw=true)
*Location management interface for creating printer toolhead and storage location QR codes*

## Prerequisites

- A PrusaLink-compatible 3D printer (Prusa or any printer with a PrusaLink API)
- Enable PrusaLink on your printer(s) for local network access, and copy the password
- Spoolman running somewhere
- **For building from source**: Go 1.25 or higher
- **(Optional) For NFC features**: NFC-capable smartphone and NFC tags (NTAG213/215/216 recommended)
- **(Recommendation) NFC Tools Pro** mobile app (for programming tags)

## Installation

### Option 1: Docker (Easiest)

1. **Run Spoolman** (if not already running):
   ```bash
   docker run -d --name spoolman -p 8000:8000 -v spoolman-data:/home/spoolman/data ghcr.io/donkie/spoolman:latest
   ```

2. **Run FilaBridge**:
   ```bash
   docker run -d --name filabridge -p 5000:5000 \
     -v "$(pwd)/data:/app/data" \
     ghcr.io/sargonas/filabridge:latest
   ```

3. **Configure**: Open `http://localhost:5000` and click "⚙️ Configuration"

**Using docker-compose (recommended for full stack):**
```yaml
services:
  filabridge:
    image: ghcr.io/sargonas/filabridge:latest
    ports:
      - "5000:5000"
    volumes:
      - ./data:/app/data
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://127.0.0.1:5000/healthz"]
      interval: 30s
      timeout: 5s
      start_period: 10s
      retries: 3
```

The Docker image sets `FILABRIDGE_DB_PATH` to `/app/data`, so the database persists in the mounted volume.

### Option 2: Pre-built Binary (Linux)

FilaBridge is built to run as a Docker container or a Linux service alongside a Spoolman instance. Pre-built binaries are provided for Linux only, for anyone who prefers to run it bare-metal as a systemd service.

1. **Download the latest release** for your architecture from the [Releases page](https://github.com/sargonas/filabridge/releases)
   - Linux (amd64, arm64)

2. **Make it executable**:
   ```bash
   chmod +x filabridge
   ```

3. **Run Spoolman** (if not already running):
   ```bash
   docker run -d --name spoolman -p 8000:8000 -v spoolman-data:/home/spoolman/data ghcr.io/donkie/spoolman:latest
   ```

4. **Start FilaBridge**:
   ```bash
   ./filabridge
   ```

5. **Configure**: Open `http://localhost:5000` and click "Configuration"

### Option 3: Build from Source

1. **Clone and build**:
   ```bash
   git clone https://github.com/sargonas/filabridge.git
   cd filabridge
   go mod download
   go build -o filabridge .
   ```

2. **Run Spoolman** (if not already running):

3. **Start FilaBridge**:
   ```bash
   ./filabridge
   ```

## Security

FilaBridge is designed for use on a private, trusted network and has no built-in authentication. For the current time being, anyone who can reach the web interface can change settings and read the credentials you have configured (PrusaLink API keys and the optional Spoolman password). Do not expose FilaBridge directly to the internet. If you need remote access, put it behind a VPN or an authenticating reverse proxy.

## Configuration

The system stores all configuration in the SQLite database. For Docker deployments, you can optionally set the `FILABRIDGE_DB_PATH` environment variable to specify where the database should be stored (defaults to `/app/data` in Docker), however I recommend leaving it as is and changing the volume mount instead, if needed.

### First Run

1. Start the application
2. Open the web interface at `http://localhost:5000`
3. Click "Start Configuration" button
4. Enter a name for your Printer.
5. Enter your PrusaLink IP Address and API key (also called the password)
6. Choose the number of toolheads your printer has.
7. Click "Save Configuration"
8. Settings are applied immediately, no restart needed

## Updating and Backups

All FilaBridge state (configuration, printer definitions, toolhead mappings, and print history) lives in a single SQLite database: `filabridge.db` in the data directory (`/app/data` in Docker).

**Updating (Docker):**
```bash
docker compose pull
docker compose up -d
```

Database schema changes are applied automatically on startup. You can check the running version in the dashboard footer, via `/healthz`, or with `./filabridge --version`.

**Backups:** Copy the data directory. The database runs in WAL mode, so alongside `filabridge.db` you may also see `filabridge.db-wal` and `filabridge.db-shm`; you could copy all three together, but it is highly recommended to first stop FilaBridge for a clean single-file snapshot.

## Usage

### Running the Service

```bash
# Run both bridge service and web interface (recommended)
./filabridge

# Custom port
./filabridge --port 8080
```

### Web Interface

The web interface provides:

- **Printer Status**: Real-time view of printer states and current jobs with live WebSocket updates
- **Toolhead Mapping**: Assign filament spools to specific toolheads with smart search functionality
- **Live Updates**: Real-time status updates without page refreshes
- **Spool Search**: Search and filter spools by ID, material, brand, or name
- **Error Management**: View and acknowledge print processing errors
- **Auto-mapping**: Automatic spool assignment when selecting from dropdowns

### Filament Management

1. **Add spools/filaments/locations to Spoolman**: Use Spoolman's web interface to add these entities
2. **Map spools to toolheads**: Use the FilaBridge web interface to assign spools with smart search
3. **Monitor usage**: The system automatically tracks and updates filament usage
4. **Handle errors**: Acknowledge any print processing errors that require manual intervention

### NFC Tag / QR Code Management

1. **Generate QR Codes**: Navigate to NFC Management tab in the web interface
2. **Create Tags**: 
   - **Spool Tags**: Generate QR codes for individual spools
   - **Filament Tags**: Generate QR codes for filament types (for new unopened spools)
   - **Location Tags**: Create and generate QR codes for printer toolheads and custom locations (dryboxes, storage shelves, etc.)
3. **Program NFC Tags**: Use a mobile NFC tool to scan QR codes and write URLs to NFC tags
4. **Assign Spools**: Tap spool tag, then location tag (location then spool works as well) to instantly assign and update inventory

If you have exactly one printer with one toolhead configured, the Spool Tags screen also offers a Quick-Assign variant for each spool: a single tag that assigns the spool directly to your printer in one scan, with no location tag needed. Multi-toolhead users can build the same thing manually by appending `&location=<location name>` to a spool URL.

## API Endpoints

The web interface also provides REST API endpoints:

- `GET /healthz` - Health check (used by the Docker healthcheck)
- `GET /api/status` - Get current printer status and mappings
- `GET /api/spools` - Get all spools from Spoolman
- `GET /api/filaments` - Get all filament types from Spoolman
- `POST /api/map_toolhead` - Map a spool to a toolhead (pass `spool_id: 0` to unmap)
- `GET /api/print-history` - Get recent print history
- `GET /api/print-errors` - Get all unacknowledged print errors
- `POST /api/print-errors/{id}/acknowledge` - Acknowledge a print error
- `GET/POST /api/printers`, `PUT/DELETE /api/printers/{id}` - Manage printer configurations
- `GET/POST /api/config` - Read and update configuration
- `GET /api/nfc/assign` - Handle NFC tag scans (spool, location, or both in one URL)
- `GET /api/nfc/urls` - Get all NFC URLs with QR codes
- `GET /api/nfc/session/status` - Check NFC session status
- `GET/POST /api/locations`, `PUT/DELETE /api/locations/{name}` - Manage locations
- `WS /ws/status` - WebSocket endpoint for real-time status updates

## Project Structure

```
filabridge/
├── main.go                # Application entry point
├── config.go              # Configuration management
├── constants.go           # Printer states, defaults, and config keys
├── logging.go             # Log routing (info to stdout, errors to stderr)
├── prusalink.go           # PrusaLink API client
├── spoolman.go            # Spoolman API client
├── bridge.go              # Core monitoring and usage tracking logic
├── nfc.go                 # NFC session management and tag handling
├── web.go                 # HTTP server and API routes
├── templates/             # HTML templates
├── static/                # CSS and JavaScript for the dashboard
├── go.mod / go.sum        # Go module definition
└── README.md              # Documentation
```

## Troubleshooting

### Common Issues

1. **Printers not accessible**:
   - Check IP addresses in the web interface configuration
   - Verify the PrusaLink password on the printer is set as the API key
   - Ensure PrusaLink is enabled on your printer(s)
   - Verify network connectivity

2. **Spoolman connection failed**:
   - Make sure Spoolman is running
   - Check the Spoolman URL in the web interface configuration
   - Verify Spoolman is accessible at the specified URL

3. **Filament usage not tracked**:
   - Ensure spools are mapped to toolheads
   - Check that prints are completing (not just pausing)
   - Verify PrusaLink API is returning filament usage data

4. **WebSocket connection issues**:
   - Check browser console for WebSocket connection errors
   - Ensure no firewall is blocking WebSocket connections
   - The dashboard reconnects automatically if the connection drops; reload the page if it gives up after repeated failures

5. **Print processing errors**:
   - Check the error notifications in the web interface
   - Acknowledge errors after manually updating Spoolman
   - Review logs in docker for detailed error information

6. **NFC tag issues**:
   - Ensure NFC tags are NTAG213, NTAG215, or NTAG216 format
   - Use a mobile NFC app such as NFC Tools Pro to verify tag is properly formatted
   - QR codes encode the full URL - scan with NFC Tools Pro to program tags
   - Sessions expire after 5 minutes - complete both scans within that timeout

### Logs

The service logs important events to the console. Look for:
- Printer status updates
- Filament usage calculations
- Spoolman update confirmations
- WebSocket connection status
- Print processing errors
- Error messages

## Development

### Building from Source

```bash
# Download dependencies
go mod download

# Build the application
go build -o filabridge .

# Run tests
go test ./...

# Run with race detection
go run -race .
```

## Contributing

Contributions are welcome! Here's how you can help:

- **Report bugs**: Open an issue with details about the problem
- **Suggest features**: Share your ideas for improvements
- **Submit PRs**: Fix bugs or add features (please open an issue first for major changes)
- **Improve docs**: Help make the documentation clearer
- **Star the repo**: Show your support!

See [CONTRIBUTING.md](CONTRIBUTING.md) for detailed guidelines.

## Roadmap

- [ ] CI builds and automated tests on pull requests
- [ ] Mask stored credentials (PrusaLink API keys, Spoolman password) in API responses
- [ ] Faster dashboard loads when a printer is offline (cache last-known printer status)
- [ ] Mobile-responsive UI improvements
- [ ] Make printer history an optional toggle, it's borderline scope creep
- [ ] Support for additional printer APIs (this one is quite the stretch goal!)

## Support the Project

If you find FilaBridge useful:
- Star the repository
- Report bugs and suggest features
- Share it with the 3D printing community
- Contribute code or documentation
- [Buy me a coffee](https://buymeacoffee.com/sargonas), or 3! ;)

## License

This project is licensed under the GNU General Public License v3.0 - see the [LICENSE](LICENSE) file for details.

## Support

For issues specific to:
- **PrusaLink**: Check Prusa's documentation
- **Spoolman**: Visit the [Spoolman GitHub repository](https://github.com/Donkie/Spoolman)
- **This bridge**: Open an issue in this repository
