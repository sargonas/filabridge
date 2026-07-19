// FilaBridge Dashboard - Dropdown Functionality

// setDropdownButton renders the standard swatch + label + arrow button content
function setDropdownButton(button, color, multiColor, text, arrow) {
    button.innerHTML = `
        <div style="display: flex; align-items: center; gap: 10px;">
            <div class="color-swatch" style="background: ${swatchBackground(color, multiColor)};"></div>
            <span>${text}</span>
        </div>
        <span class="dropdown-arrow">${arrow}</span>
    `;
}

// closeDropdown collapses a custom dropdown; clearSearch also resets its search box
function closeDropdown(dropdown, clearSearch = false) {
    dropdown.querySelector('.dropdown-content')?.classList.remove('show');
    dropdown.querySelector('.dropdown-button')?.classList.remove('open');
    dropdown.querySelector('.dropdown-arrow')?.classList.remove('open');
    if (clearSearch) {
        const searchInput = dropdown.querySelector('.dropdown-search');
        if (searchInput) {
            searchInput.value = '';
            searchInput.dispatchEvent(new Event('input'));
        }
    }
}

// Load available spools for a specific dropdown
async function loadAvailableSpools(dropdown) {
    const toolheadRow = dropdown.closest('.toolhead-mapping-row');
    if (!toolheadRow) return;
    
    const printerId = toolheadRow.getAttribute('data-printer-id');
    const toolheadId = toolheadRow.getAttribute('data-toolhead-id');
    
    // Find printer name from the printer element
    const printerElement = document.querySelector(`[data-printer-id="${printerId}"]`);
    if (!printerElement) return;
    
    const printerNameElement = printerElement.querySelector('h3');
    if (!printerNameElement) return;
    
    const printerName = printerNameElement.textContent;
    
    try {
        const response = await fetch(`/api/available_spools?printer_name=${encodeURIComponent(printerName)}&toolhead_id=${toolheadId}`);
        const data = await response.json();
        
        if (data.error) {
            console.error('Error loading available spools:', data.error);
            return;
        }
        
        // Get current selection
        const hiddenInput = dropdown.querySelector('input[type="hidden"]');
        const currentSpoolId = hiddenInput ? hiddenInput.value : '';
        
        // Update dropdown options
        const optionsContainer = dropdown.querySelector('.dropdown-options-container');
        if (!optionsContainer) return;
        
        // Clear existing options except "Empty"
        const selectOption = optionsContainer.querySelector('.dropdown-option[data-value=""]');
        optionsContainer.innerHTML = '';
        if (selectOption) {
            optionsContainer.appendChild(selectOption);
        }
        
        // Add available spools
        data.spools.forEach(spool => {
            const option = document.createElement('div');
            option.className = 'dropdown-option';
            option.setAttribute('data-value', spool.id);
            option.setAttribute('data-color', spool.filament?.color_hex || '');
            option.setAttribute('data-multi-color', spool.filament?.multi_color_hexes || '');

            if (currentSpoolId && spool.id.toString() === currentSpoolId) {
                option.classList.add('selected');
            }

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
        
        // Re-add click handlers for new options
        optionsContainer.querySelectorAll('.dropdown-option').forEach(option => {
            option.addEventListener('click', async (e) => {
                e.stopPropagation();
                
                // Update button text and selected state
                const selectedText = option.querySelector('.option-text').textContent;
                const selectedColor = option.dataset.color;
                const selectedMulti = option.dataset.multiColor;
                const selectedValue = option.dataset.value;
                
                // Update hidden input value
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
        
    } catch (error) {
        console.error('Error loading available spools:', error);
    }
}

// Custom dropdown functionality
function initCustomDropdowns() {
    document.querySelectorAll('.custom-dropdown').forEach(dropdown => {
        // Skip NFC dropdowns - they have their own initialization
        if (dropdown.closest('#spool-tags-tab, #filament-tags-tab, #location-tags-tab')) {
            return;
        }
        
        const button = dropdown.querySelector('.dropdown-button');
        const content = dropdown.querySelector('.dropdown-content');
        const arrow = dropdown.querySelector('.dropdown-arrow');
        const searchInput = dropdown.querySelector('.dropdown-search');
        const optionsContainer = dropdown.querySelector('.dropdown-options-container');
        const noResults = dropdown.querySelector('.dropdown-no-results');
        
        // Initialize search functionality
        if (searchInput) {
            searchInput.addEventListener('input', (e) => {
                const searchTerm = e.target.value.toLowerCase().trim();
                const options = optionsContainer.querySelectorAll('.dropdown-option');
                let visibleCount = 0;
                
                options.forEach(option => {
                    const optionText = option.querySelector('.option-text').textContent.toLowerCase();
                    let isMatch = searchTerm === '';
                    
                    if (searchTerm !== '') {
                        // Check if search term is purely numeric
                        if (/^\d+$/.test(searchTerm)) {
                            // For numeric search, only match the ID in brackets
                            const idMatch = optionText.match(/^\[(\d+)\]/);
                            isMatch = idMatch && idMatch[1] === searchTerm;
                        } else {
                            // For text search, use word boundary matching
                            const searchRegex = new RegExp('\\b' + searchTerm.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), 'i');
                            isMatch = searchRegex.test(optionText);
                        }
                    }
                    
                    if (isMatch) {
                        option.style.display = 'flex';
                        visibleCount++;
                    } else {
                        option.style.display = 'none';
                    }
                });
                
                // Show/hide "No results" message
                if (visibleCount === 0 && searchTerm !== '') {
                    noResults.style.display = 'block';
                } else {
                    noResults.style.display = 'none';
                }
            });
        }
        
        // Handle option selection
        content.querySelectorAll('.dropdown-option').forEach(option => {
            option.addEventListener('click', async (e) => {
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
                content.querySelectorAll('.dropdown-option').forEach(opt => opt.classList.remove('selected'));
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
        
        // Load available spools when dropdown is opened
        button.addEventListener('click', async (e) => {
            e.stopPropagation();
            
            // Close other dropdowns (clearing their search boxes)
            document.querySelectorAll('.custom-dropdown').forEach(other => {
                if (other !== dropdown) {
                    closeDropdown(other, true);
                }
            });
            
            // Toggle current dropdown
            const isOpening = !content.classList.contains('show');
            content.classList.toggle('show');
            button.classList.toggle('open');
            arrow.classList.toggle('open');
            
            // Load available spools when opening dropdown
            if (isOpening) {
                await loadAvailableSpools(dropdown);
                
                // Focus search input when opening dropdown
                if (searchInput) {
                    setTimeout(() => {
                        searchInput.focus();
                    }, 10);
                }
            }
        });
    });
    
    // Close dropdowns when clicking outside (clearing their search boxes)
    document.addEventListener('click', () => {
        document.querySelectorAll('.custom-dropdown').forEach(dropdown => closeDropdown(dropdown, true));
    });
}

// Auto-map spool to toolhead when selected
async function autoMapSpool(dropdown, selectedValue, selectedText, selectedColor, selectedMulti) {
    const toolheadRow = dropdown.closest('.toolhead-mapping-row');
    if (!toolheadRow) {
        console.error('Could not find toolhead mapping row');
        return;
    }
    
    const printerId = toolheadRow.getAttribute('data-printer-id');
    const toolheadId = toolheadRow.getAttribute('data-toolhead-id');
    
    // Find printer name from the printer element
    const printerElement = document.querySelector(`[data-printer-id="${printerId}"]`);
    if (!printerElement) {
        console.error('Could not find printer element');
        return;
    }
    
    const printerNameElement = printerElement.querySelector('h3');
    if (!printerNameElement) {
        console.error('Could not find printer name element');
        return;
    }
    
    const printerName = printerNameElement.textContent;
    
    // Show loading state
    const button = dropdown.querySelector('.dropdown-button');
    const originalContent = button.innerHTML;
    setDropdownButton(button, selectedColor, selectedMulti, selectedText, '⏳');
    
    try {
        const response = await fetch('/api/map_toolhead', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({
                printer_name: printerName,
                toolhead_id: parseInt(toolheadId),
                spool_id: selectedValue === '0' ? 0 : parseInt(selectedValue)
            })
        });
        
        const data = await response.json();
        
        if (data.error) {
            // Handle conflict errors specifically
            if (data.error.includes('is already assigned to')) {
                alert(`Spool assignment conflict: ${data.error}`);
            } else {
                alert(`Error mapping spool: ${data.error}`);
            }
            
            // Revert to previous selection
            button.innerHTML = originalContent;
            // Update edit button to previous state
            updateEditButton(toolheadRow, selectedValue, selectedColor);
            return;
        }
        
        // Success - show brief success indicator
        setDropdownButton(button, selectedColor, selectedMulti, selectedText, '✅');

        // Update edit button visibility and data
        updateEditButton(toolheadRow, selectedValue, selectedColor);

        // Reset to normal state after 2 seconds
        setTimeout(() => {
            setDropdownButton(button, selectedColor, selectedMulti, selectedText, '▼');
        }, 2000);
        
        // Only remove spools from other dropdowns if we're mapping a spool (not unmapping)
        if (selectedValue !== '0') {
            // Immediately remove the mapped spool from all other dropdowns
            removeSpoolFromOtherDropdowns(selectedValue);
        }
        // Refresh all other dropdowns to update available spools (debounced so a
        // burst of mappings doesn't fire a request storm)
        debouncedRefreshAllDropdowns();
        
    } catch (error) {
        console.error('Error mapping spool:', error);
        alert('Error mapping spool: ' + error.message);
        
        // Revert to previous selection
        button.innerHTML = originalContent;
    }
}

// Immediately remove a spool from all other dropdowns
function removeSpoolFromOtherDropdowns(spoolId) {
    const allDropdowns = document.querySelectorAll('.custom-dropdown');
    
    allDropdowns.forEach(dropdown => {
        const optionsContainer = dropdown.querySelector('.dropdown-options-container');
        if (!optionsContainer) return;
        
        // Find and remove the option with the specified spool ID
        const optionToRemove = optionsContainer.querySelector(`[data-value="${spoolId}"]`);
        if (optionToRemove) {
            optionToRemove.remove();
        }
    });
}

// Debounce refreshAllDropdowns so a burst of map/unmap operations collapses to a
// single refresh instead of one /api/available_spools sweep per operation.
let refreshDropdownsTimer = null;
function debouncedRefreshAllDropdowns() {
    clearTimeout(refreshDropdownsTimer);
    refreshDropdownsTimer = setTimeout(refreshAllDropdowns, 300);
}

// Refresh all dropdowns to update available spools
async function refreshAllDropdowns() {
    // Get all dropdowns except the one that was just updated
    const allDropdowns = document.querySelectorAll('.custom-dropdown');
    
    for (const dropdown of allDropdowns) {
        // Skip if dropdown is currently open
        const content = dropdown.querySelector('.dropdown-content');
        if (content && content.classList.contains('show')) {
            continue;
        }
        
        // Refresh the available spools for this dropdown
        await loadAvailableSpools(dropdown);
    }
}

// Update edit button visibility and data based on selected spool
function updateEditButton(toolheadRow, selectedValue, selectedColor = '') {
    const editButton = toolheadRow.querySelector('.edit-spool-btn');
    if (!editButton) return;
    
    if (selectedValue && selectedValue !== '' && selectedValue !== '0') {
        // Show button and update spool ID
        editButton.classList.remove('hidden');
        editButton.setAttribute('data-spool-id', selectedValue);
        editButton.setAttribute('onclick', `openSpoolmanEdit(${selectedValue})`);
        
        // Set button color to match filament color
        if (selectedColor) {
            editButton.style.backgroundColor = '#' + selectedColor;
            editButton.style.borderColor = '#' + selectedColor;
        } else {
            // Fallback to default blue if no color
            editButton.style.backgroundColor = '#007bff';
            editButton.style.borderColor = '#007bff';
        }
    } else {
        // Hide button
        editButton.classList.add('hidden');
        editButton.setAttribute('data-spool-id', '');
        editButton.setAttribute('onclick', 'openSpoolmanEdit(null)');
    }
}

// Open Spoolman edit page for a spool
function openSpoolmanEdit(spoolId) {
    if (!spoolId) {
        console.warn('No spool ID provided for editing');
        return;
    }
    
    const spoolmanBaseURL = document.body.dataset.spoolmanUrl;
    if (!spoolmanBaseURL) {
        alert('Spoolman URL not configured. Please check your settings.');
        return;
    }
    
    const editURL = `${spoolmanBaseURL}/spool/edit/${spoolId}`;
    window.open(editURL, '_blank');
}
