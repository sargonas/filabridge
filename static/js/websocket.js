// FilaBridge Dashboard - WebSocket Functionality

// WebSocket client for real-time updates
let ws = null;
let reconnectAttempts = 0;
let maxReconnectAttempts = 10;
let reconnectDelay = 1000; // Start with 1 second

function connectWebSocket() {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const wsUrl = `${protocol}//${window.location.host}/ws/status`;
    
    try {
        ws = new WebSocket(wsUrl);
        
        ws.onopen = function(event) {
            console.log('WebSocket connected');
            reconnectAttempts = 0;
            reconnectDelay = 1000;
            updateConnectionStatus('connected');
        };
        
        ws.onmessage = function(event) {
            try {
                const data = JSON.parse(event.data);
                if (data.type === 'status_update') {
                    updateDashboard(data);
                }
            } catch (error) {
                console.error('Error parsing WebSocket message:', error);
            }
        };
        
        ws.onclose = function(event) {
            console.log('WebSocket disconnected');
            updateConnectionStatus('disconnected');
            ws = null;
            
            // Attempt to reconnect with exponential backoff
            if (reconnectAttempts < maxReconnectAttempts) {
                setTimeout(() => {
                    reconnectAttempts++;
                    reconnectDelay = Math.min(reconnectDelay * 2, 30000); // Max 30 seconds
                    console.log(`Attempting to reconnect (${reconnectAttempts}/${maxReconnectAttempts}) in ${reconnectDelay}ms`);
                    connectWebSocket();
                }, reconnectDelay);
            } else {
                console.log('Max reconnection attempts reached');
                updateConnectionStatus('failed');
            }
        };
        
        ws.onerror = function(error) {
            console.error('WebSocket error:', error);
            updateConnectionStatus('error');
        };
        
    } catch (error) {
        console.error('Failed to create WebSocket connection:', error);
        updateConnectionStatus('error');
    }
}

function updateConnectionStatus(status) {
    // Find or create connection status indicator
    let statusIndicator = document.getElementById('ws-status');
    if (!statusIndicator) {
        statusIndicator = document.createElement('div');
        statusIndicator.id = 'ws-status';
        statusIndicator.className = 'ws-status';
        document.body.appendChild(statusIndicator);
    }

    // The badge's colours are a state class rather than inline styles, so a skin
    // can restyle it (see static/css/v2/)
    switch (status) {
        case 'connected':
            statusIndicator.textContent = '🟢 Live';
            statusIndicator.className = 'ws-status ws-status-connected';
            break;
        case 'disconnected':
            statusIndicator.textContent = '🟡 Connecting...';
            statusIndicator.className = 'ws-status ws-status-connecting';
            break;
        case 'error':
        case 'failed':
            statusIndicator.textContent = '🔴 Offline';
            statusIndicator.className = 'ws-status ws-status-offline';
            break;
    }
}

function updateDashboard(data) {
    
    // Update printer statuses
    if (data.printers) {
        updatePrinterStatuses(data.printers);
    }
    
    // Update spool data
    if (data.spools) {
        updateSpoolData(data.spools);
    }
    
    // Update toolhead mappings
    if (data.toolhead_mappings) {
        updateToolheadMappings(data.toolhead_mappings);
    }
    
    // Update print errors
    updateRunoutWarnings(data.runout_warnings || []);
    updateMappingWarnings(data.mapping_warnings || []);
    if (data.print_errors) {
        updatePrintErrors(data.print_errors);
    }
}

function updatePrinterStatuses(printers) {
    Object.entries(printers).forEach(([printerId, printerData]) => {
        if (printerId === 'no_printers') return;
        
        // Find the printer element
        const printerElement = document.querySelector(`[data-printer-id="${printerId}"]`);
        if (!printerElement) return;
        
        // Update status badge
        const statusBadge = printerElement.querySelector('.status');
        if (statusBadge) {
            statusBadge.className = `status ${printerData.state}`;
            statusBadge.textContent = printerData.state;
        }
    });
}

function updateSpoolData(spools) {
    // Update spool dropdowns with new weight data
    document.querySelectorAll('.custom-dropdown').forEach(dropdown => {
        const optionsContainer = dropdown.querySelector('.dropdown-options-container');
        if (!optionsContainer) return;
        
        // Clear existing options except "Empty"
        const selectOption = optionsContainer.querySelector('.dropdown-option[data-value=""]');
        optionsContainer.innerHTML = '';
        if (selectOption) {
            optionsContainer.appendChild(selectOption);
        }
        
        // Add updated spool options
        spools.forEach(spool => {
            const option = document.createElement('div');
            option.className = 'dropdown-option';
            option.setAttribute('data-value', spool.id);
            option.setAttribute('data-color', spool.filament?.color_hex || '');
            option.setAttribute('data-multi-color', spool.filament?.multi_color_hexes || '');

            const colorSwatch = document.createElement('div');
            colorSwatch.className = 'color-swatch';
            applySwatch(colorSwatch, spool.filament?.color_hex, spool.filament?.multi_color_hexes);
            
            const optionText = document.createElement('div');
            optionText.className = 'option-text';
            optionText.textContent = `[${spool.id}] ${spool.material || 'Unknown Material'} - ${spool.brand || 'Unknown Brand'} - ${spool.name || 'Unnamed Spool'}${spool.remaining_weight != null ? ` (${Math.round(spool.remaining_weight)}g remaining)` : ''}`;
            
            option.appendChild(colorSwatch);
            option.appendChild(optionText);
            optionsContainer.appendChild(option);
        });
        
        // Add event listeners to the new options
        optionsContainer.querySelectorAll('.dropdown-option').forEach(option => {
            option.addEventListener('click', async function(e) {
                e.stopPropagation();
                
                // Update button text and selected state
                const selectedText = option.querySelector('.option-text').textContent;
                const selectedColor = option.dataset.color;
                const selectedMulti = option.dataset.multiColor;
                const selectedValue = option.dataset.value;

                // Update hidden input value
                const hiddenInput = dropdown.querySelector('input[type="hidden"]');
                if (hiddenInput) {
                    hiddenInput.value = selectedValue;
                }

                // Update selected state
                optionsContainer.querySelectorAll('.dropdown-option').forEach(opt => opt.classList.remove('selected'));
                option.classList.add('selected');

                closeDropdown(dropdown);

                // Auto-map the spool if a spool is selected (not "Empty")
                if (selectedValue && selectedValue !== '') {
                    await autoMapSpool(dropdown, selectedValue, selectedText, selectedColor, selectedMulti);
                } else {
                    // Handle empty selection - unmap the toolhead
                    await autoMapSpool(dropdown, '0', selectedText, '', '');
                }
                
                // Update edit button after selection
                const toolheadRow = dropdown.closest('.toolhead-mapping-row');
                if (toolheadRow) {
                    updateEditButton(toolheadRow, selectedValue, selectedColor);
                }
            });
        });
    });
}

function updateToolheadMappings(mappings) {
    // First, find all toolhead rows in the DOM
    const allToolheadRows = document.querySelectorAll('.toolhead-mapping-row');
    
    // Create a set of mapped toolheads for quick lookup
    const mappedToolheads = new Set();
    Object.entries(mappings).forEach(([printerId, printerMappings]) => {
        Object.entries(printerMappings).forEach(([toolheadId, mapping]) => {
            mappedToolheads.add(`${printerId}-${toolheadId}`);
        });
    });
    
    // Process all toolhead rows
    allToolheadRows.forEach(toolheadRow => {
        const printerId = toolheadRow.getAttribute('data-printer-id');
        const toolheadId = toolheadRow.getAttribute('data-toolhead-id');
        const key = `${printerId}-${toolheadId}`;
        
        // Find the dropdown
        const dropdown = toolheadRow.querySelector('.custom-dropdown');
        if (!dropdown) return;
        
        const hiddenInput = dropdown.querySelector('input[type="hidden"]');
        const dropdownButton = dropdown.querySelector('.dropdown-button');
        const optionsContainer = dropdown.querySelector('.dropdown-options-container');
        
        if (!dropdownButton) return;
        
        // Update toolhead label with display name if available
        const toolheadLabel = toolheadRow.querySelector('.toolhead-label');
        if (toolheadLabel && mappings[printerId] && mappings[printerId][toolheadId]) {
            const mapping = mappings[printerId][toolheadId];
            if (mapping.display_name) {
                toolheadLabel.textContent = mapping.display_name + ':';
            }
        }
        
        // Check if this toolhead has a mapping
        if (mappedToolheads.has(key) && mappings[printerId] && mappings[printerId][toolheadId]) {
            // Toolhead has a mapping - update it
            const mapping = mappings[printerId][toolheadId];
            const spoolId = mapping.spool_id;
            
            // Update hidden input
            if (hiddenInput) {
                hiddenInput.value = spoolId || '';
            }
            
            // Find the spool option
            if (optionsContainer && spoolId) {
                const spoolOption = optionsContainer.querySelector(`.dropdown-option[data-value="${spoolId}"]`);
                if (spoolOption) {
                    const selectedText = spoolOption.querySelector('.option-text').textContent;
                    const selectedColor = spoolOption.dataset.color;
                    const selectedMulti = spoolOption.dataset.multiColor;

                    // Update button display
                    setDropdownButton(dropdownButton, selectedColor, selectedMulti, selectedText, '▼');
                    
                    // Mark as selected
                    optionsContainer.querySelectorAll('.dropdown-option').forEach(opt => {
                        opt.classList.remove('selected');
                    });
                    spoolOption.classList.add('selected');
                    
                    // Update edit button
                    updateEditButton(toolheadRow, spoolId, selectedColor);
                    
                }
            }
        } else {
            // Toolhead has NO mapping - clear it
            if (hiddenInput) {
                hiddenInput.value = '';
            }
            
            // Set to empty state
            dropdownButton.innerHTML = `
                <span>Select a spool...</span>
                <span class="dropdown-arrow">▼</span>
            `;
            
            // Clear selected state
            if (optionsContainer) {
                optionsContainer.querySelectorAll('.dropdown-option').forEach(opt => {
                    opt.classList.remove('selected');
                });
            }
            
            // Update edit button for empty state
            updateEditButton(toolheadRow, '', '');
            
        }
    });
}

function updateRunoutWarnings(warnings) {
    const container = document.getElementById('runout-warnings-container');
    if (!container) return;

    container.innerHTML = '';

    if (!warnings || warnings.length === 0) {
        container.style.display = 'none';
        return;
    }

    container.style.display = 'block';

    warnings.forEach(w => {
        const el = document.createElement('div');
        // Same classes status.html renders server-side, so a warning looks the
        // same whether the page was loaded with it or received it live
        el.className = 'runout-warning alert alert-warning';
        el.setAttribute('data-warning-id', w.id);

        const pausedNote = w.auto_paused
            ? '<p><strong>The print has been paused.</strong> Acknowledging will resume it (or swap the spool first, then acknowledge).</p>'
            : '';
        const buttonLabel = w.auto_paused ? 'Acknowledge &amp; Resume' : 'Acknowledge';

        el.innerHTML = `
            <h4 style="margin-top: 0;">⚠️ Low Filament Warning</h4>
            <p><strong>Printer:</strong> ${w.printer_name} (Toolhead ${w.toolhead_id})</p>
            <p><strong>Spool:</strong> [${w.spool_id}] ${w.spool_name} - ${w.remaining_weight.toFixed(1)}g remaining</p>
            <p><strong>Print needs:</strong> ~${w.required_weight.toFixed(1)}g to finish</p>
            ${pausedNote}
            <button class="btn btn-warning" onclick="acknowledgeRunoutWarning('${w.id}')">${buttonLabel}</button>
        `;

        container.appendChild(el);
    });
}

function updateMappingWarnings(warnings) {
    const container = document.getElementById('mapping-warnings-container');
    if (!container) return;

    // A status broadcast arrives every poll and rebuilds these cards. Remember
    // any toolhead already picked in a dropdown so a user reading the list does
    // not have their half-made choice reset out from under them, and then submit
    // a toolhead they did not pick.
    const pending = {};
    container.querySelectorAll('.mapping-slot-select').forEach(select => {
        const card = select.closest('[data-warning-id]');
        if (card) {
            pending[card.getAttribute('data-warning-id')] = select.value;
        }
    });

    container.innerHTML = '';

    if (!warnings || warnings.length === 0) {
        container.style.display = 'none';
        return;
    }

    container.style.display = 'block';

    warnings.forEach(w => {
        const el = document.createElement('div');
        // Same classes status.html renders server-side, so a warning looks the
        // same whether the page was loaded with it or received it live
        el.className = 'mapping-warning alert alert-warning';
        el.setAttribute('data-warning-id', w.id);
        el.innerHTML = mappingWarningBody(w);

        const previous = pending[w.id];
        if (previous !== undefined) {
            const select = el.querySelector('.mapping-slot-select');
            if (select && select.querySelector(`option[value="${previous}"]`)) {
                select.value = previous;
            }
        }

        container.appendChild(el);
    });
}

// The inside of a mapping warning card, matching what status.html renders
// server-side so the two paths cannot drift. Also used to redraw a card in place
// once the toolhead has been picked.
function mappingWarningBody(w) {
    const slots = w.slots || [];
    const selected = w.assigned ? w.assigned_toolhead : w.toolhead_id;
    const current = slots.find(s => s.toolhead_id === selected);

    let recording = '';
    if (current) {
        const spool = current.spool_label ? `, ${escapeHtml(current.spool_label)}` : '';
        recording = `<p><strong>Recording against ${escapeHtml(current.display_name)}${spool}.</strong></p>`;
        if (!current.spool_id) {
            recording += `<p>No spool is mapped to ${escapeHtml(current.display_name)}, so nothing will be recorded unless one is mapped before the print finishes.</p>`;
        }
    }

    const options = slots.map(s => {
        const label = s.spool_label ? escapeHtml(s.spool_label) : 'no spool mapped';
        return `<option value="${s.toolhead_id}"${s.toolhead_id === selected ? ' selected' : ''}>${escapeHtml(s.display_name)} - ${label}</option>`;
    }).join('');

    return `
            <h4 style="margin-top: 0;">⚠️ Confirm Toolhead Mapping</h4>
            <p><strong>Printer:</strong> ${escapeHtml(w.printer_name)}</p>
            <p><strong>Print:</strong> ${escapeHtml(w.job_name)} (~${w.grams.toFixed(1)}g)</p>
            <p>This print was sliced with a single filament, so the file does not record which slot it came from.</p>
            ${recording}
            <p>If the print is running from a different toolhead, pick it here. Nothing is recorded until the print finishes, so this can be changed until then.</p>
            <div class="mapping-warning-actions">
                <select class="mapping-slot-select" id="mapping-slot-${w.id}" aria-label="Toolhead this print is running from">
                    ${options}
                </select>
                <button class="btn btn-warning" onclick="assignMappingWarning('${w.id}')">Use this toolhead</button>
                <button class="btn btn-secondary" onclick="acknowledgeMappingWarning('${w.id}')">Dismiss</button>
            </div>
    `;
}

// Tell FilaBridge which toolhead the print is really running from, so its usage
// is recorded there instead of against the toolhead a slot-less slice defaulted
// to. Answerable, and re-answerable, until the print finishes.
async function assignMappingWarning(warningId) {
    const select = document.getElementById(`mapping-slot-${warningId}`);
    if (!select) return;

    const toolheadId = parseInt(select.value, 10);
    if (Number.isNaN(toolheadId)) return;

    try {
        const response = await fetch(`/api/mapping-warnings/${encodeURIComponent(warningId)}/assign`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ toolhead_id: toolheadId }),
        });

        const data = await response.json().catch(() => ({}));
        if (response.ok && data.warning) {
            // Redraw from the server's copy rather than waiting for the next
            // broadcast, so the confirmation is immediate.
            const el = document.querySelector(`[data-warning-id="${warningId}"]`);
            if (el) {
                el.innerHTML = mappingWarningBody(data.warning);
            }
        } else {
            alert('Failed to assign toolhead: ' + (data.error || 'Unknown error'));
        }
    } catch (error) {
        console.error('Error assigning mapping warning toolhead:', error);
        alert('Failed to assign toolhead');
    }
}

// Acknowledge an unknown-filament-slot warning
async function acknowledgeMappingWarning(warningId) {
    try {
        const response = await fetch(`/api/mapping-warnings/${encodeURIComponent(warningId)}/acknowledge`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
        });

        if (response.ok) {
            const el = document.querySelector(`[data-warning-id="${warningId}"]`);
            if (el) {
                el.remove();
            }
        } else {
            const data = await response.json().catch(() => ({}));
            alert('Failed to acknowledge warning: ' + (data.error || 'Unknown error'));
        }
    } catch (error) {
        console.error('Error acknowledging mapping warning:', error);
        alert('Failed to acknowledge warning');
    }
}

// Acknowledge a low-filament warning (resumes the print if it was auto-paused)
async function acknowledgeRunoutWarning(warningId) {
    try {
        const response = await fetch(`/api/runout-warnings/${encodeURIComponent(warningId)}/acknowledge`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
        });

        if (response.ok) {
            const el = document.querySelector(`[data-warning-id="${warningId}"]`);
            if (el) {
                el.remove();
            }
        } else {
            const data = await response.json().catch(() => ({}));
            alert('Failed to acknowledge warning: ' + (data.error || 'Unknown error'));
        }
    } catch (error) {
        alert('Failed to acknowledge warning: ' + error.message);
    }
}

function updatePrintErrors(printErrors) {
    const container = document.getElementById('print-errors-container');
    if (!container) return;
    
    // Clear existing errors
    container.innerHTML = '';
    
    if (printErrors.length === 0) {
        container.style.display = 'none';
        return;
    }
    
    container.style.display = 'block';
    
    // Add each error
    printErrors.forEach(error => {
        const errorElement = document.createElement('div');
        // Matches the server-rendered markup in status.html
        errorElement.className = 'print-error alert alert-danger';
        errorElement.setAttribute('data-error-id', error.id);
        
        const timestamp = new Date(error.timestamp).toLocaleString();
        
        errorElement.innerHTML = `
            <h4 style="margin-top: 0;">⚠️ Print Processing Failed</h4>
            <p><strong>Printer:</strong> ${error.printer_name}</p>
            <p><strong>File:</strong> ${error.filename}</p>
            <p><strong>Time:</strong> ${timestamp}</p>
            <p><strong>Error:</strong> ${error.error}</p>
            <p><strong>Action Required:</strong> Please update Spoolman manually with the correct filament usage for this print.</p>
            <button class="btn btn-danger" onclick="acknowledgeError('${error.id}')">Acknowledge</button>
        `;
        
        container.appendChild(errorElement);
    });
}

// Acknowledge print error
async function acknowledgeError(errorId) {
    try {
        const response = await fetch(`/api/print-errors/${encodeURIComponent(errorId)}/acknowledge`, {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
            },
        });

        if (response.ok) {
            // Remove the error from the UI
            const errorElement = document.querySelector(`[data-error-id="${errorId}"]`);
            if (errorElement) {
                errorElement.remove();
            }
            
            // Check if there are any remaining errors
            const remainingErrors = document.querySelectorAll('.print-error');
            if (remainingErrors.length === 0) {
                const container = document.getElementById('print-errors-container');
                if (container) {
                    container.style.display = 'none';
                }
            }
        } else {
            const data = await response.json().catch(() => ({}));
            alert('Failed to acknowledge error: ' + (data.error || 'Unknown error'));
        }
    } catch (error) {
        console.error('Error acknowledging print error:', error);
        alert('Failed to acknowledge error: ' + error.message);
    }
}
