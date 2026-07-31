// FilaBridge Dashboard - Printer Management Functions

// Helper function to escape HTML attribute values to prevent XSS
function escapeHtmlAttribute(value) {
    if (value == null) return '';
    const div = document.createElement('div');
    div.textContent = value;
    return div.innerHTML.replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

// Printer Management Functions
function loadPrinters() {
    fetch('/api/printers')
        .then(response => response.json())
        .then(data => {
            const printerList = document.getElementById('printer-list');
            printerList.innerHTML = '';
            
            if (data.printers && Object.keys(data.printers).length > 0) {
                for (const [printerId, printer] of Object.entries(data.printers)) {
                    if (printerId === 'no_printers') continue;
                    
                    const printerCard = document.createElement('div');
                    printerCard.className = 'printer-card';
                    
                    // Build toolhead names section
                    let toolheadNamesHTML = '';
                    const toolheadNames = printer.toolhead_names || {};
                    for (let toolheadID = 0; toolheadID < (printer.toolheads || 1); toolheadID++) {
                        const currentName = toolheadNames[toolheadID] || `Toolhead ${toolheadID}`;
                        const escapedName = escapeHtmlAttribute(currentName);
                        toolheadNamesHTML += `
                            <div class="form-row" style="margin-bottom: 10px;">
                                <label style="min-width: 120px;">Toolhead ${toolheadID}:</label>
                                <input type="text" 
                                       id="toolhead-name-${printerId}-${toolheadID}" 
                                       value="${escapedName}" 
                                       class="toolhead-name-input"
                                       data-printer-id="${printerId}"
                                       data-toolhead-id="${toolheadID}"
                                       style="flex: 1; padding: 8px; border-radius: 4px; border: 1px solid #666; background: rgba(255,255,255,0.1); color: #fff;">
                            </div>
                        `;
                    }
                    
                    const isBambu = printer.type === 'bambu';
                    printerCard.innerHTML = `
                        <h3>${printer.name || 'Unknown Printer'}</h3>
                        <div class="printer-info">
                            ${isBambu ? '<div><strong>Type:</strong> Bambu Lab</div>' : ''}
                            <div><strong>Toolheads:</strong> ${printer.toolheads || 1}</div>
                            <div><strong>Address:</strong> ${printer.ip_address || 'Not configured'}</div>
                            <div><strong>${isBambu ? 'Access Code' : 'API Key'}:</strong> ${printer.api_key_set ? '••••••••' : 'Not configured'}</div>
                            ${isBambu && printer.serial ? `<div><strong>Serial:</strong> ${escapeHtmlAttribute(printer.serial)}</div>` : ''}
                        </div>
                        <div class="printer-actions">
                            <button class="btn btn-small" onclick="editPrinter('${printerId}')">✏️ Edit</button>
                            <button class="btn btn-small" onclick="toggleToolheadNames('${printerId}')">🔤 Rename Toolheads</button>
                            <button class="btn btn-small btn-danger" onclick="deletePrinter('${printerId}')">🗑️ Delete</button>
                        </div>
                        <div id="toolhead-names-${printerId}" class="toolhead-names-section" style="display: none; margin-top: 15px; padding: 15px; background: rgba(255,255,255,0.05); border-radius: 5px;">
                            <h4 style="margin-top: 0; margin-bottom: 15px;">Toolhead Names</h4>
                            ${toolheadNamesHTML}
                            <div style="margin-top: 15px; text-align: right;">
                                <button class="btn btn-small" onclick="saveToolheadNames('${printerId}')">💾 Save Names</button>
                                <button class="btn btn-small btn-secondary" onclick="cancelToolheadNames('${printerId}')">❌ Cancel</button>
                            </div>
                        </div>
                    `;
                    printerList.appendChild(printerCard);
                }
            } else {
                printerList.innerHTML = '<div class="printer-card"><p>No printers configured. Click "Add Printer" to get started.</p></div>';
            }
        })
        .catch(error => {
            console.error('Error loading printers:', error);
            document.getElementById('printer-list').innerHTML = '<div class="printer-card"><p>Error loading printers. Please refresh the page.</p></div>';
        });
}

// applyPrinterTypeUI adjusts a printer form for the selected type: it shows the
// serial field and relabels the credential field ("Access Code" vs "API Key")
// for Bambu printers. ids names the elements for the add or edit form.
function applyPrinterTypeUI(type, ids) {
    const isBambu = type === 'bambu';

    const serialGroup = document.getElementById(ids.serialGroup);
    if (serialGroup) serialGroup.style.display = isBambu ? 'block' : 'none';
    const serialInput = document.getElementById(ids.serial);
    if (serialInput) serialInput.required = isBambu;

    const apiLabel = document.getElementById(ids.apiLabel);
    if (apiLabel) {
        const star = ids.apiRequired ? ' *' : '';
        apiLabel.textContent = (isBambu ? 'Access Code' : 'API Key') + star;
    }
    const apiHelp = document.getElementById(ids.apiHelp);
    if (apiHelp) {
        apiHelp.textContent = isBambu
            ? 'LAN access code from the printer (Settings → Network → LAN Only Mode).'
            : ids.prusaHelp;
    }
}

const ADD_PRINTER_IDS = {
    serialGroup: 'printerSerialGroup', serial: 'printerSerial',
    apiLabel: 'printerAPIKeyLabel', apiHelp: 'printerAPIKeyHelp',
    apiRequired: true, prusaHelp: 'Found in PrusaLink settings on your printer'
};
const EDIT_PRINTER_IDS = {
    serialGroup: 'editPrinterSerialGroup', serial: 'editPrinterSerial',
    apiLabel: 'editPrinterAPIKeyLabel', apiHelp: 'editPrinterAPIKeyHelp',
    apiRequired: false, prusaHelp: 'Never displayed once saved. Leave blank to keep the current key.'
};

function showAddPrinterForm() {
    document.getElementById('addPrinterModal').style.display = 'block';
    document.getElementById('addPrinterForm').reset();

    // The printer-type selector (and Bambu support) is only offered in
    // developer mode; otherwise every printer is a PrusaLink.
    const typeGroup = document.getElementById('printerTypeGroup');
    const typeSelect = document.getElementById('printerType');
    if (typeSelect) typeSelect.value = 'prusalink';
    if (typeGroup) typeGroup.style.display = (typeof isDeveloperMode === 'function' && isDeveloperMode()) ? 'block' : 'none';
    applyPrinterTypeUI('prusalink', ADD_PRINTER_IDS);

    // Reset button state AFTER form reset with a fresh query
    // Use setTimeout to ensure DOM is updated
    setTimeout(() => {
        const submitButton = document.querySelector('#addPrinterForm button[type="submit"]');
        if (submitButton) {
            submitButton.disabled = false;
            submitButton.textContent = 'Add Printer';
        }
    }, 0);
}

function closeAddPrinterModal() {
    document.getElementById('addPrinterModal').style.display = 'none';
    
    // Ensure button state is reset when modal is closed
    const submitButton = document.querySelector('#addPrinterForm button[type="submit"]');
    if (submitButton) {
        submitButton.disabled = false;
        submitButton.textContent = 'Add Printer';
    }
}

function closeEditPrinterModal() {
    document.getElementById('editPrinterModal').style.display = 'none';
    
    // Ensure button state is reset when modal is closed
    const submitButton = document.querySelector('#editPrinterForm button[type="submit"]');
    if (submitButton) {
        submitButton.disabled = false;
        submitButton.textContent = 'Update Printer';
    }
}

// Close modal when clicking outside of it
window.onclick = function(event) {
    const addModal = document.getElementById('addPrinterModal');
    const editModal = document.getElementById('editPrinterModal');
    if (event.target == addModal) {
        closeAddPrinterModal();
    } else if (event.target == editModal) {
        closeEditPrinterModal();
    }
}

function addPrinter(printerConfig) {
    return apiRequest('/api/printers', { method: 'POST', body: printerConfig });
}

// Toggle the add form's serial/credential fields when the printer type changes.
(function () {
    const typeSelect = document.getElementById('printerType');
    if (typeSelect) {
        typeSelect.addEventListener('change', function () {
            applyPrinterTypeUI(this.value, ADD_PRINTER_IDS);
        });
    }
})();

// Handle form submission
document.getElementById('addPrinterForm').addEventListener('submit', function(e) {
    e.preventDefault();
    
    // Check if form is valid before proceeding
    if (!this.checkValidity()) {
        // Form has validation errors, don't change button state
        return;
    }
    
    const formData = new FormData(this);
    const name = formData.get('name');
    const ipAddress = formData.get('ip_address');
    const apiKey = formData.get('api_key');
    const toolheads = parseInt(formData.get('toolheads'));
    // Type is only selectable in developer mode; default to PrusaLink otherwise.
    const type = (typeof isDeveloperMode === 'function' && isDeveloperMode())
        ? (formData.get('type') || 'prusalink') : 'prusalink';
    const serial = formData.get('serial') || '';

    // Show loading state
    const submitButton = this.querySelector('button[type="submit"]');
    const originalText = submitButton.textContent;
    submitButton.disabled = true;
    submitButton.textContent = 'Adding...';

    const config = {
        name: name,
        ip_address: ipAddress,
        api_key: apiKey,
        toolheads: toolheads,
        type: type
    };
    if (type === 'bambu') config.serial = serial;
    addPrinter(config)
    .then(() => {
        // Success - close modal and refresh
        closeAddPrinterModal();
        loadPrinters();
    })
    .catch(error => {
        // Reset button state
        submitButton.disabled = false;
        submitButton.textContent = originalText;
        alert('Error adding printer: ' + error.message);
    });
});

// Handle edit form submission
document.getElementById('editPrinterForm').addEventListener('submit', function(e) {
    e.preventDefault();
    
    const formData = new FormData(this);
    const printerId = formData.get('printerId');
    const name = formData.get('name');
    const ipAddress = formData.get('ip_address');
    const apiKey = formData.get('api_key');
    const toolheads = parseInt(formData.get('toolheads'));
    // Carry the printer's type through the edit so a Bambu printer is not
    // silently reverted to PrusaLink; serial only matters for Bambu.
    const type = formData.get('type') || 'prusalink';
    const serial = formData.get('serial') || '';

    // Validate printerId is present
    if (!printerId) {
        alert('Error: Printer ID is missing. Please try again.');
        return;
    }

    // Show loading state
    const submitButton = this.querySelector('button[type="submit"]');
    if (!submitButton) {
        alert('Error: Submit button not found.');
        return;
    }

    const originalText = submitButton.textContent || 'Update Printer';
    submitButton.disabled = true;
    submitButton.textContent = 'Updating...';

    // Create printer config
    const printerConfig = {
        name: name,
        ip_address: ipAddress,
        api_key: apiKey,
        toolheads: toolheads,
        type: type
    };
    if (type === 'bambu') printerConfig.serial = serial;
    
    // Update the printer
    apiRequest(`/api/printers/${printerId}`, { method: 'PUT', body: printerConfig })
    .then(() => {
        // Success - close modal and refresh
        closeEditPrinterModal();
        loadPrinters();
    })
    .catch(error => {
        // Reset button state - ensure it always happens
        if (submitButton) {
            submitButton.disabled = false;
            submitButton.textContent = originalText;
        }
        alert('Error updating printer: ' + error.message);
    });
});

function editPrinter(printerId) {
    // Get the current printer data
    fetch('/api/printers')
        .then(response => response.json())
        .then(data => {
            const printer = data.printers[printerId];
            if (!printer) {
                alert('Printer not found');
                return;
            }
            
            // Populate the edit form with current data
            const type = printer.type || 'prusalink';
            document.getElementById('editPrinterId').value = printerId;
            document.getElementById('editPrinterType').value = type;
            document.getElementById('editPrinterName').value = printer.name || '';
            document.getElementById('editPrinterIP').value = printer.ip_address || '';
            document.getElementById('editPrinterSerial').value = printer.serial || '';
            document.getElementById('editPrinterAPIKey').value = ''; // keys are never returned by the API
            document.getElementById('editPrinterToolheads').value = printer.toolheads || 1;
            applyPrinterTypeUI(type, EDIT_PRINTER_IDS);

            // Show the edit modal
            document.getElementById('editPrinterModal').style.display = 'block';
        })
        .catch(error => {
            console.error('Error loading printer data:', error);
            alert('Error loading printer data');
        });
}

function deletePrinter(printerId) {
    if (confirm('Are you sure you want to delete this printer?')) {
        apiRequest(`/api/printers/${printerId}`, { method: 'DELETE' })
        .then(() => {
            alert('Printer deleted successfully!');
            loadPrinters();
        })
        .catch(error => {
            alert('Error deleting printer: ' + error.message);
        });
    }
}

// Toolhead Name Management Functions
function toggleToolheadNames(printerId) {
    const section = document.getElementById(`toolhead-names-${printerId}`);
    if (section.style.display === 'none') {
        section.style.display = 'block';
        // Store original values when opening
        const inputs = section.querySelectorAll('.toolhead-name-input');
        inputs.forEach(input => {
            input.dataset.originalValue = input.value;
        });
    } else {
        section.style.display = 'none';
    }
}

function saveToolheadNames(printerId) {
    const section = document.getElementById(`toolhead-names-${printerId}`);
    const inputs = section.querySelectorAll('.toolhead-name-input');
    const updates = [];
    
    // Collect all updates
    inputs.forEach(input => {
        const toolheadId = parseInt(input.dataset.toolheadId);
        const newName = input.value.trim();
        const originalName = input.dataset.originalValue || '';
        
        // Only update if name changed
        if (newName !== originalName && newName !== '') {
            updates.push({
                toolheadId: toolheadId,
                name: newName
            });
        }
    });
    
    if (updates.length === 0) {
        alert('No changes to save');
        return;
    }
    
    // Save each toolhead name
    const savePromises = updates.map(update =>
        apiRequest(`/api/printers/${printerId}/toolheads/${update.toolheadId}`, {
            method: 'PUT',
            body: { name: update.name }
        })
    );
    
    // Execute all updates
    Promise.all(savePromises)
        .then(() => {
            alert('Toolhead names saved successfully!');
            // Close the section and reload printers to show updated names
            section.style.display = 'none';
            loadPrinters();
        })
        .catch(error => {
            alert('Error saving toolhead names: ' + error.message);
        });
}

function cancelToolheadNames(printerId) {
    const section = document.getElementById(`toolhead-names-${printerId}`);
    const inputs = section.querySelectorAll('.toolhead-name-input');
    
    // Restore original values
    inputs.forEach(input => {
        if (input.dataset.originalValue) {
            input.value = input.dataset.originalValue;
        }
    });
    
    section.style.display = 'none';
}
