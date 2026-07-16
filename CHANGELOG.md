# Changelog

All notable changes to FilaBridge will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v0.9.4] - 2026-07-15

### Fixed

- capture filament estimate at print start via streaming file scan

### Changed

- remove printer model field and hostname detection since the model is not used anywhere functionally and is only for display purposes

### Other

- Merge pull request #16 from sargonas/consumed-bugfix
- Merge pull request #12 from sargonas/QoL-improvements

## [v0.9.3] - 2026-07-14

### Other

- Merge pull request #11 from sargonas/QoL-improvements
- ops feat: pure-Go sqlite driver, native multi-arch builds, merged release workflow, all to streamline from 2x 15min builds to a single flow under 5min
- Merge pull request #10 from sargonas/QoL-improvements
- cleaned up legacy "billing" language every where in favor of "recording" and updated Readme
- Merge pull request #4 from sargonas/QoL-improvements
- i hate css... ugh.. okay dual QR code layout fixed!
- build CI fixes

## [v0.9.2] - 2026-07-14

### Fixed

- shutdown race, websocket hub race, auto-assign toggle, cancelled-print overbilling, missing error page

### Other

- Merge pull request #3 from sargonas/QoL-improvements
- release versions are properly tagged fro docker with pre release in mind
- removed old code that self writes release notes, to manage them manually better.
- heavy logging clean up
- QoL pass 2:
- QoL Pass for docker mode only, and project take-over housekeeping

## [v0.9.1] - 2026-07-14

### Added

- track filament for cancelled prints and make billing reliable
- add URL copy functionality and properly encode NFC URLs.
- accept hostnames or IP addresses for Spoolman and Printers.
- enhance settings UI with sub-tabs for better organization and add functionality for automatic spool assignment with location selection
- implement auto-assignment of previous spools with configuration options and API endpoints
- add toolhead name management with custom display names and API endpoints for retrieval and updates
- embed static files into the binary and update routing to serve them
- add support for Spoolman basic authentication with username and password
- add static files directory to Dockerfile for improved asset management
- implement NFC management features including QR code generation and location tracking
- add edit button for spools
- filter out spools with 0g remaining weight in GetAllSpools method
- add local time conversion for error timestamps in print processing notifications
- Add advanced timeout settings for PrusaLink and Spoolman API, enhancing configuration flexibility in the UI
- Enhance spool display by including IDs in the UI and improve search functionality for numeric IDs
- Refactor toolhead mapping logic to handle unmapping and improve dropdown functionality for spool selection
- Update default Spoolman URL and enhance logging for printer info retrieval and model detection
- Enhance toolhead mapping functionality by adding auto-mapping for selected spools
- Add spool assignment conflict checking and available spools retrieval in the web interface, enhancing toolhead mapping functionality
- Integrate WebSocket support for real-time status updates in the web interface, including client connection management and dashboard updates
- Add search functionality to dropdown in index.html with styling and no-results handling
- Implement mutex locking for printer configuration operations and enhance config loading process
- Add Docker support with Dockerfile, docker-compose, and CI workflow

### Fixed

- enhance error logging and improve DNS resolution timeout for PrusaLink client
- Update location management to reflect API limitations
- add HTML escaping for toolhead names to prevent XSS vulnerabilities
- handle null values for remaining weight in spool display across dropdowns and NFC tags
- identify and skip virtual printer toolhead locations in location management
- round remaining weight in spool tag details for improved display
- removing web port input
- implement error ID sanitization for URL safety in print error handling
- add copying of static files in Dockerfile to streamline asset deployment
- properly encode error ID in fetch request for acknowledging print errors
- enhance print processing logic in FilamentBridge to prevent duplicate handling and improve state management
- streamline print completion handling in monitorPrusaLink, removing files/jobs being processed duplicate times.
- reduce Spoolman timeout from 30 seconds to 10 seconds for improved performance
- fix not being able to dismiss error messages
- Increase file download timeout to accommodate slower USB storage and enhance logging for timeout configuration
- Standardize error messages in print processing for consistency and clarity
- Implement print error handling with acknowledgment feature and enhance UI for displaying print errors
- Enhance database configuration to support environment variable for database path

### Changed

- update Dockerfile to use --no-scripts flag for apk to address Alpine 3.23 trigger script issues
- update Dockerfile to include apk update before installing dependencies
- migrate location management from FilaBridge to Spoolman, removing legacy location functions and updating related API endpoints
- improve event listener management for auto-assign previous spool checkbox
- refactor CHANGELOG generation in release workflow to use printf for header and new entry creation
- add .yamllint configuration to allow relaxed validation for workflow files. Try to correct CHANGELOG generation.
- support OrcaSlicer generated gcode files as well as PrusaSlicer bgcode files.
- fix changelog entry for v0.1.0
- update CHANGELOG and enhance README with additional screenshots
- enhance changelog generation to categorize commits by type
- update CHANGELOG for v0.0.13, removing outdated v0.0.11 entry
- enhance CHANGELOG generation by categorizing commits and improving file handling
- add workflow_dispatch trigger to docker-build and release workflows
- improve handling of commits in CHANGELOG generation to support newlines and special characters
- fixing the way CHANGELOG and Release Notes are generated.
- update changelog  documenting enhancements
- Optimize Dockerfile and GitHub Actions for improved build caching and layer management
- Adjust padding in index.html for improved layout consistency
- Introduce constants for configuration keys and default values, streamline printer state handling, and enhance filament usage processing
- Update docker-compose to use pre-built image for filabridge
- Remove README.md and update .github/README.md with Docker installation instructions
- Update Go base image in Dockerfile from 1.21-alpine to 1.23-alpine
- Improve changelog generation and update release workflow for better handling of detached HEAD state
- Remove CHANGELOG.md and update changelog generation process in release workflow
- Add configuration for conventional commits and changelog generation

### Documentation

- Update README to use direct link for dashboard screenshot, improving accessibility
- Update README to reflect new features including WebSocket live updates, enhanced spool search, and print error handling
- Update README.md to clarify database path configuration for Docker deployments

### Other

- Modify funding configuration in FUNDING.yml
- split logging between stderr and stdout instead of all stdout and only log one printer connection fail util it comes back online
- Merge pull request #1 from sargonas/feat/cancelled-print-tracking
- Update README.md
- Update README.md
- Merge branch 'main' of https://github.com/needo37/filabridge
- Refactor IsFirstRun logic to check printer_configs instead of configuration table
- Update release.yml
- Remove duplicate connection error display from index.html template
- Refactor template loading to use embedded filesystem for HTML templates
- Update README.md
- Update README.md
- Initial commit

## [v0.2.4] - 2025-12-08

### Fixed

- enhance error logging and improve DNS resolution timeout for PrusaLink client

## [v0.2.3.1] - 2025-12-05

### Changed

- update Dockerfile to use --no-scripts flag for apk to address Alpine 3.23 trigger script issues

## [v0.2.3] - 2025-12-05

### Changed

- update Dockerfile to include apk update before installing dependencies

## [v0.2.2] - 2025-12-05

### Added

- add URL copy functionality and properly encode NFC URLs.

### Fixed

- Update location management to reflect API limitations

### Changed

- migrate location management from FilaBridge to Spoolman, removing legacy location functions and updating related API endpoints

## [v0.2.1] - 2025-12-03

### Added

- accept hostnames or IP addresses for Spoolman and Printers.

## [v0.2] - 2025-12-03

### Added

- enhance settings UI with sub-tabs for better organization and add functionality for automatic spool assignment with location selection
- implement auto-assignment of previous spools with configuration options and API endpoints
- add toolhead name management with custom display names and API endpoints for retrieval and updates

### Fixed

- add HTML escaping for toolhead names to prevent XSS vulnerabilities
- handle null values for remaining weight in spool display across dropdowns and NFC tags
- identify and skip virtual printer toolhead locations in location management
- round remaining weight in spool tag details for improved display

### Changed

- improve event listener management for auto-assign previous spool checkbox

## [v0.1.5] - 2025-11-18

### Added

- embed static files into the binary and update routing to serve them

### Changed

- refactor CHANGELOG generation in release workflow to use printf for header and new entry creation

## [v0.1.3] - 2025-11-02

### Fixed

- implement error ID sanitization for URL safety in print error handling

## [v0.1.2] - 2025-10-21

### Fixed

- add copying of static files in Dockerfile to streamline asset deployment

## [v0.1.1] - 2025-10-21

### Added

- add static files directory to Dockerfile for improved asset management

### Changed

- update CHANGELOG and enhance README with additional screenshots

## [v0.1.0] - 2025-10-21

### Added

- implement NFC management features including QR code generation and location tracking

## [v0.0.15] - 2025-10-20

### Added

- add edit button for spools
- filter out spools with 0g remaining weight in GetAllSpools method

### Changed

- enhance changelog generation to categorize commits by type

## [v0.0.14] - 2025-10-15

### Added

- fix: properly encode error ID in fetch request for acknowledging print errors
- feat: add local time conversion for error timestamps in print processing notifications
- chore(release): update CHANGELOG for v0.0.13, removing outdated v0.0.11 entry
- fix: enhance print processing logic in FilamentBridge to prevent duplicate handling and improve state management
- chore(release): update changelog for v0.0.13


### Added

- bug: streamline print completion handling in monitorPrusaLink, removing files/jobs being processed duplicate times.
- fix: reduce Spoolman timeout from 30 seconds to 10 seconds for improved performance
- chore(release): update changelog for v0.0.12

## [v0.0.12] - 2025-10-14

### Added

- bug: fix not being able to dismiss error messages
- docs: Update README to use direct link for dashboard screenshot, improving accessibility
- chore(release): enhance CHANGELOG generation by categorizing commits and improving file handling
- chore(release): update changelog for v0.0.11

### Added

- feat: Add advanced timeout settings for PrusaLink and Spoolman API, enhancing configuration flexibility in the UI
- chore(release): update changelog for v0.0.10
