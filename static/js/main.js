// FilaBridge Dashboard - Main JavaScript Functions

// Tab switching functionality
function switchTab(tabName) {
    // Hide all tab contents
    const tabContents = document.querySelectorAll('.tab-content');
    tabContents.forEach(content => {
        content.classList.remove('active');
    });
    
    // Remove active class from all tabs
    const tabs = document.querySelectorAll('.tab');
    tabs.forEach(tab => {
        tab.classList.remove('active');
    });
    
    // Show selected tab content
    document.getElementById(tabName + '-tab').classList.add('active');
    
    // Add active class to clicked tab
    event.target.classList.add('active');
    
    // Load print history when its tab is opened
    if (tabName === 'history') {
        loadPrintHistory();
    }

    // Load configuration when settings tab is opened
    if (tabName === 'settings') {
        // Load data for the currently active settings sub-tab
        const activeSettingsTab = document.querySelector('.settings-tab.active');
        if (activeSettingsTab) {
            // Determine which tab is active and load its data
            const activeTabContent = document.querySelector('.settings-tab-content.active');
            if (activeTabContent) {
                const tabId = activeTabContent.id.replace('-tab', '');
                if (tabId === 'getting-started') {
                    // Getting Started tab doesn't need data loading
                } else if (tabId === 'basic-config') {
                    loadConfiguration();
                } else if (tabId === 'printers') {
                    loadPrinters();
                } else if (tabId === 'advanced') {
                    loadAdvancedSettings();
                    loadAutoAssignSettings();
                    loadPrintHistorySettings();
                }
            }
        }
    }
}

function toggleConfig() {
    // Switch to the settings tab
    switchTab('settings');
}

// Print History tab
async function loadPrintHistory() {
    const container = document.getElementById('history-table-container');
    try {
        // Fetch history and spools together; spools are only used to show
        // friendly names, so a spool that no longer exists falls back to its ID
        const [historyRes, spoolsRes] = await Promise.all([
            fetch('/api/print-history'),
            fetch('/api/spools').catch(() => null)
        ]);
        const historyData = await historyRes.json();

        const spoolInfo = {};
        if (spoolsRes && spoolsRes.ok) {
            const spools = await spoolsRes.json();
            spools.forEach(s => {
                spoolInfo[s.id] = {
                    name: s.name,
                    colorHex: s.filament?.color_hex || '',
                    multiColorHexes: s.filament?.multi_color_hexes || '',
                    multiColorDirection: s.filament?.multi_color_direction || ''
                };
            });
        }

        const history = historyData.history || [];
        if (history.length === 0) {
            container.innerHTML = '<p>No prints recorded yet. Completed and cancelled prints will appear here once filament usage has been tracked.</p>';
            return;
        }

        const rows = history.map(h => {
            const finished = new Date(h.print_finished);
            const started = new Date(h.print_started);
            const durationMs = finished - started;
            const info = spoolInfo[h.spool_id];
            const spoolLabel = info
                ? `[${h.spool_id}] ${escapeHtml(info.name)}`
                : `Spool #${h.spool_id}`;
            const swatchHtml = info ? buildColorSwatchHTML(info.colorHex, info.multiColorHexes, info.multiColorDirection) : '';
            const statusClass = h.status === 'completed' ? 'history-status-completed' : 'history-status-cancelled';
            const statusLabel = h.status === 'completed' ? '✅ Completed' : '🛑 Cancelled/Failed';
            return `
                <tr>
                    <td class="history-job">${escapeHtml(h.job_name)}</td>
                    <td>${escapeHtml(h.printer_name)}</td>
                    <td><span class="history-status ${statusClass}">${statusLabel}</span></td>
                    <td><div style="display: flex; align-items: center; gap: 8px;">${swatchHtml}<span>${spoolLabel}</span></div></td>
                    <td>${h.filament_used.toFixed(1)}g</td>
                    <td>${finished.toLocaleString()}</td>
                    <td>${formatDuration(durationMs)}</td>
                </tr>`;
        }).join('');

        container.innerHTML = `
            <div class="history-table-wrapper">
                <table class="history-table">
                    <thead>
                        <tr>
                            <th>Job</th>
                            <th>Printer</th>
                            <th>Status</th>
                            <th>Spool</th>
                            <th>Filament</th>
                            <th>Finished</th>
                            <th>Run Time</th>
                        </tr>
                    </thead>
                    <tbody>${rows}</tbody>
                </table>
            </div>`;
    } catch (error) {
        console.error('Error loading print history:', error);
        container.innerHTML = '<p>Error loading print history</p>';
    }
}

// formatDuration renders a millisecond span as "3h 24m" / "12m" / "45s"
function formatDuration(ms) {
    if (!isFinite(ms) || ms < 0) return '—';
    const totalMinutes = Math.floor(ms / 60000);
    const hours = Math.floor(totalMinutes / 60);
    const minutes = totalMinutes % 60;
    if (hours > 0) return `${hours}h ${minutes}m`;
    if (totalMinutes > 0) return `${minutes}m`;
    return `${Math.floor(ms / 1000)}s`;
}

function escapeHtml(text) {
    const div = document.createElement('div');
    div.textContent = text == null ? '' : String(text);
    return div.innerHTML;
}

// Settings sub-tab switching functionality
function switchSettingsTab(tabName, clickedElement) {
    // Hide all settings tab contents
    document.querySelectorAll('.settings-tab-content').forEach(tab => {
        tab.classList.remove('active');
    });
    
    // Remove active class from all settings tabs
    document.querySelectorAll('.settings-tab').forEach(tab => {
        tab.classList.remove('active');
    });
    
    // Show selected tab content
    const targetTab = document.getElementById(tabName + '-tab');
    if (targetTab) {
        targetTab.classList.add('active');
    }
    
    // Add active class to clicked tab
    if (clickedElement) {
        clickedElement.classList.add('active');
    } else {
        // Fallback: find the tab button by onclick attribute
        const tabButtons = document.querySelectorAll('.settings-tab');
        tabButtons.forEach(btn => {
            if (btn.getAttribute('onclick') && btn.getAttribute('onclick').includes(tabName)) {
                btn.classList.add('active');
            }
        });
    }
    
    // Load data for specific tabs
    if (tabName === 'getting-started') {
        // Getting Started tab doesn't need data loading
    } else if (tabName === 'basic-config') {
        loadConfiguration();
    } else if (tabName === 'printers') {
        loadPrinters();
    } else if (tabName === 'advanced') {
        loadAdvancedSettings();
        loadAutoAssignSettings();
        loadPrintHistorySettings();
    }
}

// Configuration Management
function loadConfiguration() {
    fetch('/api/config')
        .then(response => response.json())
        .then(config => {
            const form = document.getElementById('config-form');
            form.innerHTML = `
                <div style="max-width: 600px; margin: 0 auto;">
                    <div class="form-group">
                        <label><strong>Spoolman URL:</strong></label>
                        <input type="text" id="spoolman_url" value="${config.spoolman_url || ''}" placeholder="http://localhost:8000">
                        <small>URL where Spoolman is running</small>
                    </div>
                    <div class="form-group">
                        <label><strong>Spoolman Username (optional):</strong></label>
                        <input type="text" id="spoolman_username" value="${config.spoolman_username || ''}" placeholder="Leave empty if not using basic auth">
                        <small>Username for Spoolman basic authentication (optional)</small>
                    </div>
                    <div class="form-group">
                        <label><strong>Spoolman Password (optional):</strong></label>
                        <input type="password" id="spoolman_password" value="" placeholder="${config.spoolman_password_set === 'true' ? 'Unchanged - enter a new value to replace' : 'Leave empty if not using basic auth'}">
                        <small>Never displayed once saved. Leave blank to keep the current password; clear the username to disable authentication.</small>
                    </div>
                    <div class="form-group">
                        <label><strong>Poll Interval (seconds):</strong></label>
                        <input type="number" id="poll_interval" value="${config.poll_interval || '30'}" min="10" max="300">
                        <small>How often to check printer status</small>
                    </div>
                    <div class="form-group">
                        <label style="display: flex; align-items: center; gap: 10px; cursor: pointer;">
                            <input type="checkbox" id="runout_warning_enabled" style="width: auto; cursor: pointer;" ${config.runout_warning_enabled !== 'false' ? 'checked' : ''}>
                            <span><strong>Low filament warning</strong></span>
                        </label>
                        <small>Warn on the dashboard when the loaded spool has less filament remaining than the print requires. Purely informational.</small>
                    </div>
                    <div class="form-group">
                        <label style="display: flex; align-items: center; gap: 10px; cursor: pointer;">
                            <input type="checkbox" id="runout_pause_enabled" style="width: auto; cursor: pointer;" ${config.runout_pause_enabled === 'true' ? 'checked' : ''}>
                            <span><strong>Pause print on low filament warning</strong></span>
                        </label>
                        <small>Also pause the print when the warning fires. Acknowledging the warning resumes the print (or continues as normal if you already resumed it at the printer).</small>
                    </div>
                    <div style="margin-top: 20px; text-align: center;">
                        <button class="btn" onclick="saveConfiguration()">💾 Save Configuration</button>
                    </div>
                </div>
            `;
        })
        .catch(error => {
            console.error('Error loading configuration:', error);
            document.getElementById('config-form').innerHTML = '<p style="color: red;">Error loading configuration</p>';
        });
}

function saveConfiguration() {
    const config = {
        spoolman_url: document.getElementById('spoolman_url').value,
        spoolman_username: document.getElementById('spoolman_username').value,
        poll_interval: document.getElementById('poll_interval').value,
        runout_warning_enabled: document.getElementById('runout_warning_enabled').checked ? 'true' : 'false',
        runout_pause_enabled: document.getElementById('runout_pause_enabled').checked ? 'true' : 'false'
    };
    
    const password = document.getElementById('spoolman_password').value;
    if (password) {
        config.spoolman_password = password;
    }

    fetch('/api/config', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify(config)
    })
    .then(response => response.json())
    .then(data => {
        if (data.error) {
            alert('Error saving configuration: ' + data.error);
        } else {
            alert('Configuration saved successfully! The application will restart.');
            location.reload();
        }
    })
    .catch(error => {
        alert('Error saving configuration: ' + error.message);
    });
}

// Advanced Settings Functions
function loadAdvancedSettings() {
    fetch('/api/config')
        .then(response => response.json())
        .then(config => {
            document.getElementById('prusalinkTimeout').value = config.prusalink_timeout || '10';
            document.getElementById('prusalinkFileDownloadTimeout').value = config.prusalink_file_download_timeout || '60';
            document.getElementById('spoolmanTimeout').value = config.spoolman_timeout || '30';
        })
        .catch(error => {
            console.error('Error loading advanced settings:', error);
        });
}

function saveAdvancedSettings() {
    const config = {
        prusalink_timeout: document.getElementById('prusalinkTimeout').value,
        prusalink_file_download_timeout: document.getElementById('prusalinkFileDownloadTimeout').value,
        spoolman_timeout: document.getElementById('spoolmanTimeout').value
    };
    
    // Validate inputs
    if (config.prusalink_timeout < 5 || config.prusalink_timeout > 300) {
        alert('PrusaLink API timeout must be between 5 and 300 seconds');
        return;
    }
    if (config.prusalink_file_download_timeout < 10 || config.prusalink_file_download_timeout > 600) {
        alert('File download timeout must be between 10 and 600 seconds');
        return;
    }
    if (config.spoolman_timeout < 5 || config.spoolman_timeout > 300) {
        alert('Spoolman API timeout must be between 5 and 300 seconds');
        return;
    }
    
    fetch('/api/config', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify(config)
    })
    .then(response => response.json())
    .then(data => {
        if (data.error) {
            alert('Error saving advanced settings: ' + data.error);
        } else {
            alert('Advanced settings saved successfully! The application will restart to apply changes.');
            location.reload();
        }
    })
    .catch(error => {
        alert('Error saving advanced settings: ' + error.message);
    });
}

function resetAdvancedSettings() {
    if (confirm('Reset all timeout settings to their default values?')) {
        document.getElementById('prusalinkTimeout').value = '10';
        document.getElementById('prusalinkFileDownloadTimeout').value = '60';
        document.getElementById('spoolmanTimeout').value = '30';
    }
}

// Auto-Assign Previous Spool Settings Functions
// Store the checkbox change handler so we can remove it before adding a new one
let autoAssignCheckboxHandler = null;

function loadAutoAssignSettings() {
    // First, load the settings
    fetch('/api/config/auto-assign-previous-spool')
        .then(response => response.json())
        .then(data => {
            if (data.error) {
                console.error('Error loading auto-assign settings:', data.error);
                return;
            }
            
            const enabled = data.enabled || false;
            const location = data.location || '';
            
            document.getElementById('autoAssignPreviousSpoolEnabled').checked = enabled;
            
            // Show/hide location dropdown based on checkbox
            const locationGroup = document.getElementById('autoAssignLocationGroup');
            if (locationGroup) {
                locationGroup.style.display = enabled ? 'block' : 'none';
            }
            
            // Load locations and populate dropdown
            return fetch('/api/locations')
                .then(response => response.json())
                .then(locationsData => {
                    if (locationsData.error) {
                        console.error('Error loading locations:', locationsData.error);
                        return;
                    }
                    
                    const locationSelect = document.getElementById('autoAssignPreviousSpoolLocation');
                    if (!locationSelect) return;
                    
                    // Clear existing options except the first one
                    locationSelect.innerHTML = '<option value="">Select a location...</option>';
                    
                    // Filter out printer toolhead locations (we only want storage locations)
                    const storageLocations = locationsData.locations.filter(loc => {
                        return !loc.is_virtual && loc.type !== 'printer';
                    });
                    
                    // Sort locations alphabetically by name
                    storageLocations.sort((a, b) => {
                        const nameA = (a.name || '').toLowerCase();
                        const nameB = (b.name || '').toLowerCase();
                        return nameA.localeCompare(nameB);
                    });
                    
                    // Add locations to dropdown
                    storageLocations.forEach(loc => {
                        const option = document.createElement('option');
                        option.value = loc.name;
                        option.textContent = loc.name;
                        if (loc.name === location) {
                            option.selected = true;
                        }
                        locationSelect.appendChild(option);
                    });
                    
                    // If the saved location is not in the list (e.g., it was deleted), add it as selected
                    if (location && !storageLocations.find(loc => loc.name === location)) {
                        const option = document.createElement('option');
                        option.value = location;
                        option.textContent = location + ' (not found)';
                        option.selected = true;
                        locationSelect.appendChild(option);
                    }
                })
                .catch(error => {
                    console.error('Error loading locations:', error);
                });
        })
        .then(() => {
            // Set up checkbox change handler
            const checkbox = document.getElementById('autoAssignPreviousSpoolEnabled');
            const locationGroup = document.getElementById('autoAssignLocationGroup');
            
            if (checkbox && locationGroup) {
                // Remove existing event listener if it exists
                if (autoAssignCheckboxHandler) {
                    checkbox.removeEventListener('change', autoAssignCheckboxHandler);
                }
                
                // Create and store the new handler function
                autoAssignCheckboxHandler = function() {
                    locationGroup.style.display = this.checked ? 'block' : 'none';
                };
                
                // Add the event listener
                checkbox.addEventListener('change', autoAssignCheckboxHandler);
            }
        })
        .catch(error => {
            console.error('Error loading auto-assign settings:', error);
        });
}

function saveAutoAssignSettings() {
    const enabled = document.getElementById('autoAssignPreviousSpoolEnabled').checked;
    const locationSelect = document.getElementById('autoAssignPreviousSpoolLocation');
    const location = locationSelect ? locationSelect.value.trim() : '';
    
    const settings = {
        enabled: enabled,
        location: location
    };
    
    fetch('/api/config/auto-assign-previous-spool', {
        method: 'PUT',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify(settings)
    })
    .then(response => response.json())
    .then(data => {
        if (data.error) {
            alert('Error saving auto-assign settings: ' + data.error);
        } else {
            alert('Auto-assign settings saved successfully!');
        }
    })
    .catch(error => {
        alert('Error saving auto-assign settings: ' + error.message);
    });
}

// Print History settings
let printHistoryWasEnabled = true;

function loadPrintHistorySettings() {
    fetch('/api/config/print-history')
        .then(response => response.json())
        .then(data => {
            const checkbox = document.getElementById('printHistoryEnabled');
            if (checkbox) {
                checkbox.checked = data.enabled !== false;
                printHistoryWasEnabled = data.enabled !== false;
            }
        })
        .catch(error => {
            console.error('Error loading print history settings:', error);
        });
}

function savePrintHistorySettings() {
    const checkbox = document.getElementById('printHistoryEnabled');
    const enabled = checkbox.checked;

    // Reassure on the way out: turning history off hides the tab but keeps the data
    if (!enabled && printHistoryWasEnabled) {
        const proceed = confirm('The Print History tab will be hidden and new prints will not be logged. Existing entries are kept and will return if you re-enable this. Continue?');
        if (!proceed) {
            checkbox.checked = true;
            return;
        }
    }

    fetch('/api/config/print-history', {
        method: 'PUT',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({ enabled: enabled })
    })
    .then(response => response.json())
    .then(data => {
        if (data.error) {
            alert('Error saving history settings: ' + data.error);
        } else {
            // The tab is rendered server-side, so a reload applies the change
            window.location.reload();
        }
    })
    .catch(error => {
        alert('Error saving history settings: ' + error.message);
    });
}

function clearPrintHistory() {
    if (!confirm('Delete all stored print history? This cannot be undone. (Spoolman data is not affected.)')) {
        return;
    }
    fetch('/api/print-history', { method: 'DELETE' })
    .then(response => response.json())
    .then(data => {
        if (data.error) {
            alert('Error clearing history: ' + data.error);
        } else {
            alert('Print history cleared.');
        }
    })
    .catch(error => {
        alert('Error clearing history: ' + error.message);
    });
}

// Utility Functions
function apiUrl(path) {
    // Ensure path starts with / if not already
    if (!path.startsWith('/')) {
        path = '/' + path;
    }
    return `${window.location.origin}${path}`;
}

function buildColorSwatchHTML(colorHex, multiColorHexes, multiColorDirection) {
    if (multiColorHexes) {
        const colors = multiColorHexes.split(',');
        const dir = multiColorDirection || 'coaxial';
        return '<div class="color-swatch-multi ' + dir + '">' +
            colors.map(c => '<div class="color-stripe" style="background-color: #' + c.trim() + ';"></div>').join('') +
            '</div>';
    }
    return '<div class="color-swatch" style="background-color: #' + (colorHex || 'ccc') + ';"></div>';
}

function buildColorSwatch(colorHex, multiColorHexes, multiColorDirection) {
    if (multiColorHexes) {
        const colors = multiColorHexes.split(',');
        const container = document.createElement('div');
        container.className = 'color-swatch-multi ' + (multiColorDirection || 'coaxial');
        colors.forEach(c => {
            const stripe = document.createElement('div');
            stripe.className = 'color-stripe';
            stripe.style.backgroundColor = '#' + c.trim();
            container.appendChild(stripe);
        });
        return container;
    }
    const swatch = document.createElement('div');
    swatch.className = 'color-swatch';
    swatch.style.backgroundColor = '#' + (colorHex || 'ccc');
    return swatch;
}

function buildColorStyle(colorHex, multiColorHexes, multiColorDirection) {
    if (multiColorHexes) {
        const colors = multiColorHexes.split(',').map(c => '#' + c.trim());
        const direction = multiColorDirection === 'longitudinal' ? 'to bottom' : 'to right';
        const gradient = 'linear-gradient(' + direction + ', ' + colors.join(', ') + ')';
        return { background: gradient, borderColor: colors[0] };
    }
    const color = '#' + (colorHex || '007bff');
    return { background: color, borderColor: color };
}

function initColorSwatches() {
    document.querySelectorAll('.color-swatch[data-color]').forEach(swatch => {
        const multiHexes = swatch.getAttribute('data-multi-color-hexes');
        const multiDir = swatch.getAttribute('data-multi-color-direction');
        if (multiHexes) {
            const multiSwatch = buildColorSwatch(null, multiHexes, multiDir);
            swatch.replaceWith(multiSwatch);
        } else {
            const color = swatch.getAttribute('data-color');
            if (color) {
                swatch.style.backgroundColor = '#' + color;
            }
        }
    });
}

function initEditButtonColors() {
    document.querySelectorAll('.edit-spool-btn[data-color-hex], .edit-spool-btn[data-multi-color-hexes]').forEach(button => {
        const style = buildColorStyle(
            button.getAttribute('data-color-hex'),
            button.getAttribute('data-multi-color-hexes'),
            button.getAttribute('data-multi-color-direction')
        );
        button.style.background = style.background;
        button.style.borderColor = style.borderColor;
    });
}

// Convert server timestamps to local time
function convertTimestampsToLocal() {
    const timestampElements = document.querySelectorAll('.error-timestamp');
    timestampElements.forEach(element => {
        const timestampData = element.getAttribute('data-timestamp');
        if (timestampData) {
            const localTime = new Date(timestampData).toLocaleString();
            element.textContent = localTime;
        }
    });
}

// Initialize everything when page loads
document.addEventListener('DOMContentLoaded', function() {
    convertTimestampsToLocal();
    connectWebSocket();
    loadNfcData();
    loadPrinters();
    initCustomDropdowns();
    initColorSwatches();
    initEditButtonColors();
});
