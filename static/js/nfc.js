// FilaBridge Dashboard - NFC Management Functions

// NFC Management Functions
function switchNfcTab(tabName, clickedElement) {
    // Hide all NFC tab contents
    document.querySelectorAll('.nfc-tab-content').forEach(tab => {
        tab.classList.remove('active');
    });
    
    // Remove active class from all NFC tabs
    document.querySelectorAll('.nfc-tab').forEach(tab => {
        tab.classList.remove('active');
    });
    
    // Show selected tab content
    document.getElementById(tabName + '-tab').classList.add('active');
    
    // Add active class to clicked tab
    if (clickedElement) {
        clickedElement.classList.add('active');
    } else {
        // Fallback: find the tab button by onclick attribute
        const tabButtons = document.querySelectorAll('.nfc-tab');
        tabButtons.forEach(btn => {
            if (btn.getAttribute('onclick').includes(tabName)) {
                btn.classList.add('active');
            }
        });
    }
    
    // Load data for specific tabs
    if (tabName === 'spool-tags') {
        loadSpoolTags();
    } else if (tabName === 'filament-tags') {
        loadFilamentTags();
    } else if (tabName === 'location-tags') {
        loadLocationTags();
    }
}

// fetchNfcUrls fetches all NFC tag URLs (spools, filaments, locations) in one call
async function fetchNfcUrls() {
    const response = await fetch('/api/nfc/urls');
    return response.json();
}

async function loadNfcData() {
    // One shared fetch for all three panes instead of three identical requests
    const shared = fetchNfcUrls();
    await Promise.all([loadSpoolTags(shared), loadFilamentTags(shared), loadLocationTags(shared)]);
}

async function loadSpoolTags(dataPromise) {
    try {
        const data = await (dataPromise || fetchNfcUrls());

        const container = document.getElementById('spool-list-container');
        const spoolUrls = data.urls.filter(url => url.type === 'spool');
        
        if (spoolUrls.length === 0) {
            container.innerHTML = '<p>No spools available</p>';
            return;
        }
        
        container.innerHTML = '';
        
        spoolUrls.forEach(url => {
            const item = document.createElement('div');
            item.className = 'nfc-list-item';
            item.dataset.value = url.spool_id;
            item.dataset.color = url.color_hex;
            item.dataset.url = url.url;
            item.dataset.qr = url.qr_code_base64;
            
            const colorHex = url.color_hex || '#ccc';
            item.innerHTML = `
                <div class="color-swatch" style="background-color: ${colorHex}"></div>
                <div class="item-info">
                    <div class="item-name">[${url.spool_id}] ${url.spool_name}</div>
                    <div class="item-details">${url.material} - ${url.brand}${url.remaining_weight != null ? ` - ${Math.round(url.remaining_weight)}g remaining` : ''}</div>
                </div>
            `;
            
            // Add click handler
            item.addEventListener('click', () => {
                // Remove selected class from all items
                container.querySelectorAll('.nfc-list-item').forEach(i => i.classList.remove('selected'));
                // Add selected class to clicked item
                item.classList.add('selected');
                // Show QR code
                displaySpoolQR(url);
            });
            
            container.appendChild(item);
        });
        
        // Initialize search functionality
        initNfcSearch('spool-search', 'spool-list-container');

    } catch (error) {
        console.error('Error loading spool tags:', error);
        document.getElementById('spool-list-container').innerHTML = '<p>Error loading spools</p>';
    }
}

async function loadFilamentTags(dataPromise) {
    try {
        const data = await (dataPromise || fetchNfcUrls());

        const container = document.getElementById('filament-list-container');
        const filamentUrls = data.urls.filter(url => url.type === 'filament');
        
        if (filamentUrls.length === 0) {
            container.innerHTML = '<p>No filaments available</p>';
            return;
        }
        
        container.innerHTML = '';
        
        filamentUrls.forEach(url => {
            const item = document.createElement('div');
            item.className = 'nfc-list-item';
            item.dataset.value = url.filament_id;
            item.dataset.color = url.color_hex;
            item.dataset.url = url.url;
            item.dataset.qr = url.qr_code_base64;
            
            const colorHex = url.color_hex || '#ccc';
            item.innerHTML = `
                <div class="color-swatch" style="background-color: ${colorHex}"></div>
                <div class="item-info">
                    <div class="item-name">${url.filament_name}</div>
                    <div class="item-details">${url.material} - ${url.brand}</div>
                </div>
            `;
            
            // Add click handler
            item.addEventListener('click', () => {
                // Remove selected class from all items
                container.querySelectorAll('.nfc-list-item').forEach(i => i.classList.remove('selected'));
                // Add selected class to clicked item
                item.classList.add('selected');
                // Show QR code
                displayFilamentQR(url);
            });
            
            container.appendChild(item);
        });
        
        // Initialize search functionality
        initNfcSearch('filament-search', 'filament-list-container');

    } catch (error) {
        console.error('Error loading filament tags:', error);
        document.getElementById('filament-list-container').innerHTML = '<p>Error loading filaments</p>';
    }
}

async function loadLocationTags(dataPromise) {
    try {
        const data = await (dataPromise || fetchNfcUrls());

        const container = document.getElementById('location-list-container');
        const locationUrls = data.urls.filter(url => url.type === 'location');
        
        // Clear container and add informational message
        container.innerHTML = '';
        
        // Add informational banner about Spoolman locations
        const spoolmanURL = data.spoolman_url || '';
        const messageBanner = document.createElement('div');
        messageBanner.className = 'nfc-info-banner';
        messageBanner.style.cssText = 'background: #fff3cd; border: 1px solid #ffeaa7; color: #856404; padding: 15px; margin-bottom: 15px; border-radius: 8px;';
        
        let bannerHTML = '<strong>ℹ️ Location Management:</strong><br>';
        bannerHTML += 'It is not possible via the Spoolman API to add locations automatically. ';
        bannerHTML += 'To create locations, please do so via Spoolman. Then they will show up here.';
        
        if (spoolmanURL) {
            // Append /locations to the Spoolman URL
            const spoolmanLocationsURL = spoolmanURL.replace(/\/$/, '') + '/locations';
            bannerHTML += '<br><br><a href="' + spoolmanLocationsURL + '" target="_blank" style="color: #856404; text-decoration: underline; font-weight: bold;">Open Spoolman →</a>';
        }
        
        messageBanner.innerHTML = bannerHTML;
        container.appendChild(messageBanner);
        
        if (locationUrls.length === 0) {
            const noLocationsMsg = document.createElement('p');
            noLocationsMsg.textContent = 'No locations available. Create locations in Spoolman to see them here.';
            noLocationsMsg.style.cssText = 'padding: 20px; text-align: center; color: #666;';
            container.appendChild(noLocationsMsg);
            return;
        }
        
        locationUrls.forEach(url => {
            const item = document.createElement('div');
            item.className = 'nfc-list-item';
            item.dataset.value = url.display_name;
            item.dataset.url = url.url;
            item.dataset.qr = url.qr_code_base64;
            
            // Determine icon based on location type
            let icon = '📦'; // Storage icon for storage locations
            let iconHtml = icon;
            if (url.location_type === 'printer') {
                iconHtml = '<img src="/static/images/3d-printer-icon.png" alt="3D Printer" style="width: 20px; height: 20px;">';
            }
            
            item.innerHTML = `
                <div class="location-icon">${iconHtml}</div>
                <div class="item-info">
                    <div class="item-name">${url.display_name}</div>
                </div>
                <div class="location-actions">
                    ${renderLocationActions(url)}
                </div>
            `;
            
            // Add click handler
            item.addEventListener('click', (e) => {
                // Don't trigger if clicking on action buttons
                if (e.target.closest('.location-actions')) {
                    return;
                }
                
                // Remove selected class from all items
                container.querySelectorAll('.nfc-list-item').forEach(i => i.classList.remove('selected'));
                // Add selected class to clicked item
                item.classList.add('selected');
                // Show QR code
                displayLocationQR({
                    name: url.display_name,
                    is_printer_location: url.location_type === 'printer',
                    url: url.url,
                    qr_code_base64: url.qr_code_base64,
                    description: url.description || ''
                });
            });
            
            container.appendChild(item);
        });
        
        // Initialize search functionality
        initNfcSearch('location-search', 'location-list-container');

    } catch (error) {
        console.error('Error loading location tags:', error);
        document.getElementById('location-list-container').innerHTML = '<p>Error loading locations</p>';
    }
}

// Render inline actions for FilaBridge-managed locations
function renderLocationActions(url) {
    try {
        // Only show actions for non-printer locations (printer locations are virtual)
        if (url.location_type === 'printer') return '';
        
        const nameAttr = (url.display_name || '').replace(/'/g, "\\'").replace(/"/g, '&quot;');
        
        // Show rename for all FilaBridge locations
        let actions = `<a href="javascript:void(0)" onclick="event.preventDefault(); event.stopPropagation(); renameLocation('${nameAttr}');">Rename</a>`;
        
        // Show delete for local-only locations (not synced to Spoolman)
        if (url.is_local_only) {
            actions += ` • <a href="javascript:void(0)" onclick="event.preventDefault(); event.stopPropagation(); deleteLocation('${nameAttr}');" style="color: #ff6b6b;">Delete</a>`;
        } else {
            actions += ` <span style="color: #666; font-size: 0.9em;">(Synced to Spoolman)</span>`;
        }
        
        return `<span style="margin-left:8px; font-weight:normal;">${actions}</span>`;
    } catch (error) {
        console.error('Error rendering location actions:', error);
        return '';
    }
}

// Copy URL to clipboard
async function copyUrlToClipboard(urlElementId, buttonElement) {
    const url = document.getElementById(urlElementId).textContent;
    if (!url) {
        console.warn('No URL to copy');
        return;
    }

    // Visual feedback - change icon temporarily
    const showCopiedFeedback = () => {
        const icon = buttonElement.querySelector('.nfc-copy-icon');
        const originalIcon = icon.textContent;
        icon.textContent = '✓';
        buttonElement.style.background = 'rgba(76, 175, 80, 0.3)';
        setTimeout(() => {
            icon.textContent = originalIcon;
            buttonElement.style.background = '';
        }, 2000);
    };

    try {
        await navigator.clipboard.writeText(url);
        showCopiedFeedback();
    } catch (err) {
        // Fallback: navigator.clipboard is unavailable on plain-HTTP LAN
        // origins, which is FilaBridge's normal deployment
        console.error('Failed to copy URL:', err);
        const textArea = document.createElement('textarea');
        textArea.value = url;
        textArea.style.position = 'fixed';
        textArea.style.opacity = '0';
        document.body.appendChild(textArea);
        textArea.select();
        try {
            document.execCommand('copy');
            showCopiedFeedback();
        } catch (fallbackErr) {
            console.error('Fallback copy failed:', fallbackErr);
            alert('Failed to copy URL. Please copy manually.');
        }
        document.body.removeChild(textArea);
    }
}

// Display QR code for selected spool
function displaySpoolQR(spoolData) {
    
    // Hide no-selection message
    document.getElementById('spool-no-selection').style.display = 'none';
    
    // Show QR display
    const display = document.getElementById('spool-qr-display');
    display.style.display = 'block';
    
    // Update content
    document.getElementById('spool-selected-name').textContent = `[${spoolData.spool_id}] ${spoolData.spool_name}`;
    document.getElementById('spool-selected-details').innerHTML = ``;
    document.getElementById('spool-qr-large').src = `data:image/png;base64,${spoolData.qr_code_base64}`;
    document.getElementById('spool-url-text').textContent = spoolData.url;

    // Quick-assign variant: only present when a single printer with a single
    // toolhead is configured, so one scan can assign the spool directly
    const comboSection = document.getElementById('spool-combo-section');
    if (spoolData.combo_url) {
        document.getElementById('spool-combo-details').innerHTML =
            `Assigns this spool to <strong>${spoolData.combo_location}</strong> in a single scan, no location tag needed.`;
        document.getElementById('spool-combo-qr-large').src = `data:image/png;base64,${spoolData.combo_qr_code_base64}`;
        document.getElementById('spool-combo-url-text').textContent = spoolData.combo_url;
        // Clear the inline value rather than setting one, so the stylesheet keeps
        // control of the column's display type (grid where subgrid is supported).
        comboSection.style.display = '';
        display.classList.add('has-combo');
    } else {
        comboSection.style.display = 'none';
        display.classList.remove('has-combo');
    }
}

// Display QR code for selected filament
function displayFilamentQR(filamentData) {
    
    // Hide no-selection message
    document.getElementById('filament-no-selection').style.display = 'none';
    
    // Show QR display
    const display = document.getElementById('filament-qr-display');
    display.style.display = 'block';
    
    // Update content
    document.getElementById('filament-selected-name').textContent = filamentData.filament_name;
    document.getElementById('filament-selected-details').innerHTML = ``;
    document.getElementById('filament-qr-large').src = `data:image/png;base64,${filamentData.qr_code_base64}`;
    document.getElementById('filament-url-text').textContent = filamentData.url;
}

// Display QR code for selected location
function displayLocationQR(locationData) {
    
    // Hide no-selection message
    document.getElementById('location-no-selection').style.display = 'none';
    
    // Show QR display
    const display = document.getElementById('location-qr-display');
    display.style.display = 'block';
    
    // Update content
    document.getElementById('location-selected-name').textContent = locationData.name;
    document.getElementById('location-selected-details').innerHTML = `
        <strong>Type:</strong> ${locationData.is_printer_location ? 'Printer Location' : 'Custom Location'}<br>
        ${locationData.description ? `<strong>Description:</strong> ${locationData.description}<br>` : ''}
    `;
    document.getElementById('location-qr-large').src = `data:image/png;base64,${locationData.qr_code_base64}`;
    document.getElementById('location-url-text').textContent = locationData.url;
}

// Shared search filter for the NFC list panes: filters items in the container
// by name/details as the user types. The listener is attached once per input
// (guarded by a data attribute) so repeated tab loads don't stack handlers.
function initNfcSearch(searchId, containerId) {
    const searchInput = document.getElementById(searchId);
    if (!searchInput || searchInput.dataset.searchInit) {
        return;
    }
    searchInput.dataset.searchInit = 'true';
    const container = document.getElementById(containerId);

    searchInput.addEventListener('input', (e) => {
        const searchTerm = e.target.value.toLowerCase();
        container.querySelectorAll('.nfc-list-item').forEach(item => {
            const name = item.querySelector('.item-name')?.textContent.toLowerCase() || '';
            const details = item.querySelector('.item-details')?.textContent.toLowerCase() || '';
            item.style.display = (name.includes(searchTerm) || details.includes(searchTerm)) ? 'flex' : 'none';
        });
    });
}

// Location Management Functions
async function renameLocation(currentName) {
    const newName = prompt('Rename location', currentName || '');
    if (!newName || newName.trim() === '' || newName === currentName) return;
    try {
        const url = apiUrl(`/api/locations/${encodeURIComponent(currentName)}`);
        const res = await fetch(url, {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json', 'Accept': 'application/json' },
            mode: 'same-origin', credentials: 'same-origin',
            body: JSON.stringify({ name: newName.trim() })
        });
        if (!res.ok) {
            const errorText = await res.text();
            throw new Error(errorText);
        }
        const result = await res.json();
        await loadLocationTags();
        if (result.message) {
            alert(result.message);
        }
    } catch (e) { 
        console.error('Rename error:', e); 
        alert(e.message || 'Network error'); 
    }
}

async function deleteLocation(name) {
    try {
        const url = apiUrl(`/api/locations/${encodeURIComponent(name)}`);
        const res = await fetch(url, {
            method: 'DELETE',
            headers: { 'Accept': 'application/json' },
            mode: 'same-origin', credentials: 'same-origin'
        });
        if (!res.ok) {
            const errorText = await res.text();
            throw new Error(errorText);
        }
        const result = await res.json();
        await loadLocationTags();
    } catch (e) {
        console.error('Delete error:', e);
        alert(e.message || 'Network error');
    }
}

