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
                } else if (tabId === 'notifications') {
                    loadNotificationSettings();
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

        const spoolNames = {};
        if (spoolsRes && spoolsRes.ok) {
            const spools = await spoolsRes.json();
            spools.forEach(s => { spoolNames[s.id] = s.name; });
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
            const spoolLabel = spoolNames[h.spool_id]
                ? `[${h.spool_id}] ${spoolNames[h.spool_id]}`
                : `Spool #${h.spool_id}`;
            const statusClass = h.status === 'completed' ? 'history-status-completed' : 'history-status-cancelled';
            const statusLabel = h.status === 'completed' ? '✅ Completed' : '🛑 Cancelled/Failed';
            return `
                <tr>
                    <td class="history-job">${escapeHtml(h.job_name)}</td>
                    <td>${escapeHtml(h.printer_name)}</td>
                    <td><span class="history-status ${statusClass}">${statusLabel}</span></td>
                    <td>${escapeHtml(spoolLabel)}</td>
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
    } else if (tabName === 'notifications') {
        loadNotificationSettings();
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

// Notification Settings Functions
let appriseCheckboxHandler = null;

let _savedAppriseApiUrl = '';
let _savedAppriseUrls = '';
let _savedAppriseMode = 'stateless';
let _savedAppriseKey = '';
let _savedAppriseTag = '';

function _checkNotificationFieldsChanged() {
    const apiUrl = document.getElementById('appriseApiUrl')?.value || '';
    const urls = document.getElementById('appriseUrls')?.value || '';
    const mode = document.querySelector('input[name="appriseMode"]:checked')?.value || 'stateless';
    const key = document.getElementById('appriseKey')?.value || '';
    const tag = document.getElementById('appriseTag')?.value || '';
    const changed = apiUrl !== _savedAppriseApiUrl || urls !== _savedAppriseUrls ||
        mode !== _savedAppriseMode || key !== _savedAppriseKey || tag !== _savedAppriseTag;
    document.querySelectorAll('.test-event-btn').forEach(btn => {
        btn.style.display = changed ? 'none' : '';
    });
    if (changed) {
        document.getElementById('notificationSaveResult').innerHTML = '';
    }
}

function _updateModeFields() {
    const mode = document.querySelector('input[name="appriseMode"]:checked')?.value || 'stateless';
    document.getElementById('statelessFields').style.display = mode === 'stateless' ? 'block' : 'none';
    document.getElementById('statefulFields').style.display = mode === 'stateful' ? 'block' : 'none';
}

function _onModeChange() {
    _updateModeFields();
    _checkNotificationFieldsChanged();
}

function loadNotificationSettings() {
    fetch('/api/config/notifications')
        .then(response => response.json())
        .then(data => {
            if (data.error) {
                console.error('Error loading notification settings:', data.error);
                return;
            }

            document.getElementById('appriseEnabled').checked = data.enabled || false;
            document.getElementById('appriseApiUrl').value = data.api_url || '';
            document.getElementById('appriseUrls').value = data.urls || '';
            document.getElementById('appriseKey').value = data.key || '';
            document.getElementById('appriseTag').value = data.tag || '';

            const mode = data.mode || 'stateless';
            document.getElementById('appriseModeStateless').checked = mode === 'stateless';
            document.getElementById('appriseModeStateful').checked = mode === 'stateful';
            _updateModeFields();

            _savedAppriseApiUrl = data.api_url || '';
            _savedAppriseUrls = data.urls || '';
            _savedAppriseMode = mode;
            _savedAppriseKey = data.key || '';
            _savedAppriseTag = data.tag || '';

            document.getElementById('notifyPrintStarted').checked = data.notify_print_started !== false;
            document.getElementById('notifyPrintDone').checked = data.notify_print_done !== false;
            document.getElementById('notifyPrintFailed').checked = data.notify_print_failed !== false;
            document.getElementById('notifyLowFilament').checked = data.notify_low_filament !== false;
            document.getElementById('notifyAutoPaused').checked = data.notify_auto_paused !== false;
            document.getElementById('notifyOffline').checked = data.notify_offline !== false;
            document.getElementById('notifyOnline').checked = data.notify_online !== false;

            const settingsGroup = document.getElementById('appriseSettingsGroup');
            settingsGroup.style.display = data.enabled ? 'block' : 'none';

            const checkbox = document.getElementById('appriseEnabled');
            if (appriseCheckboxHandler) {
                checkbox.removeEventListener('change', appriseCheckboxHandler);
            }
            appriseCheckboxHandler = function() {
                settingsGroup.style.display = this.checked ? 'block' : 'none';
            };
            checkbox.addEventListener('change', appriseCheckboxHandler);

            document.getElementById('appriseConnectionResult').innerHTML = '';
            document.getElementById('notificationSaveResult').innerHTML = '';

            const apiUrlInput = document.getElementById('appriseApiUrl');
            const urlsInput = document.getElementById('appriseUrls');
            const keyInput = document.getElementById('appriseKey');
            const tagInput = document.getElementById('appriseTag');
            const modeRadios = document.querySelectorAll('input[name="appriseMode"]');

            [apiUrlInput, urlsInput, keyInput, tagInput].forEach(el => {
                el.removeEventListener('input', _checkNotificationFieldsChanged);
                el.addEventListener('input', _checkNotificationFieldsChanged);
            });
            modeRadios.forEach(r => {
                r.removeEventListener('change', _onModeChange);
                r.addEventListener('change', _onModeChange);
            });

            _checkNotificationFieldsChanged();
        })
        .catch(error => {
            console.error('Error loading notification settings:', error);
        });
}

function saveNotificationSettings() {
    const enabled = document.getElementById('appriseEnabled').checked;
    const apiUrl = document.getElementById('appriseApiUrl').value.trim();

    const resultDiv = document.getElementById('notificationSaveResult');

    if (enabled && !apiUrl) {
        resultDiv.innerHTML = '<span style="color: #f44336;">❌ Apprise API URL is required when notifications are enabled.</span>';
        return;
    }

    resultDiv.innerHTML = '<span style="color: #aaa;">Saving...</span>';

    const mode = document.querySelector('input[name="appriseMode"]:checked')?.value || 'stateless';
    const key = document.getElementById('appriseKey').value.trim() || 'apprise';
    const tag = document.getElementById('appriseTag').value.trim() || 'all';

    if (mode === 'stateful') {
        document.getElementById('appriseKey').value = key;
        document.getElementById('appriseTag').value = tag;
    }

    const settings = {
        enabled: enabled,
        api_url: apiUrl,
        mode: mode,
        urls: document.getElementById('appriseUrls').value,
        key: key,
        tag: tag,
        notify_print_started: document.getElementById('notifyPrintStarted').checked,
        notify_print_done: document.getElementById('notifyPrintDone').checked,
        notify_print_failed: document.getElementById('notifyPrintFailed').checked,
        notify_low_filament: document.getElementById('notifyLowFilament').checked,
        notify_auto_paused: document.getElementById('notifyAutoPaused').checked,
        notify_offline: document.getElementById('notifyOffline').checked,
        notify_online: document.getElementById('notifyOnline').checked,
    };

    fetch('/api/config/notifications', {
        method: 'PUT',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify(settings)
    })
    .then(response => response.json())
    .then(data => {
        if (data.error) {
            resultDiv.innerHTML = '<span style="color: #f44336;">❌ ' + escapeHtml(data.error) + '</span>';
        } else {
            resultDiv.innerHTML = '<span style="color: #4caf50;">✅ Settings saved</span>';
            _savedAppriseApiUrl = document.getElementById('appriseApiUrl').value.trim();
            _savedAppriseUrls = document.getElementById('appriseUrls').value;
            _savedAppriseMode = document.querySelector('input[name="appriseMode"]:checked')?.value || 'stateless';
            _savedAppriseKey = document.getElementById('appriseKey').value.trim();
            _savedAppriseTag = document.getElementById('appriseTag').value.trim();
            _checkNotificationFieldsChanged();
        }
    })
    .catch(error => {
        resultDiv.innerHTML = '<span style="color: #f44336;">❌ ' + escapeHtml(error.message) + '</span>';
    });
}

function testAppriseConnection() {
    const resultDiv = document.getElementById('appriseConnectionResult');
    resultDiv.innerHTML = '<span style="color: #aaa;">Testing connection...</span>';

    const apiURL = document.getElementById('appriseApiUrl')?.value || '';
    fetch('/api/config/notifications/test-connection', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({ api_url: apiURL })
    })
    .then(response => response.json())
    .then(data => {
        if (data.connected) {
            resultDiv.innerHTML = '<span style="color: #4caf50;">✅ Connection successful</span>';
        } else {
            resultDiv.innerHTML = '<span style="color: #f44336;">❌ ' + escapeHtml(data.error) + '</span>';
        }
    })
    .catch(error => {
        resultDiv.innerHTML = '<span style="color: #f44336;">❌ Connection failed: ' + escapeHtml(error.message) + '</span>';
    });
}

function testEventNotification(event) {
    const resultDiv = document.getElementById('notificationSaveResult');
    resultDiv.innerHTML = '<span style="color: #aaa;">Sending test notification...</span>';

    fetch('/api/config/notifications/test', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({ event: event })
    })
    .then(response => response.json())
    .then(data => {
        if (data.error) {
            resultDiv.innerHTML = '<span style="color: #f44336;">❌ ' + escapeHtml(data.error) + '</span>';
        } else {
            resultDiv.innerHTML = '<span style="color: #4caf50;">✅ Test notification sent</span>';
        }
    })
    .catch(error => {
        resultDiv.innerHTML = '<span style="color: #f44336;">❌ ' + escapeHtml(error.message) + '</span>';
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

// Initialize color swatches based on data-color attributes
function initColorSwatches() {
    document.querySelectorAll('.color-swatch[data-color]').forEach(swatch => {
        const color = swatch.getAttribute('data-color');
        if (color) {
            swatch.style.backgroundColor = '#' + color;
        }
    });
}

// Initialize edit button colors from data attributes
function initEditButtonColors() {
    document.querySelectorAll('.edit-spool-btn[data-color-hex]').forEach(button => {
        const colorHex = button.getAttribute('data-color-hex');
        if (colorHex) {
            button.style.backgroundColor = '#' + colorHex;
            button.style.borderColor = '#' + colorHex;
        }
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
