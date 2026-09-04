/**
 * Searchable Timezone Picker
 * Replaces native <select> with a custom searchable dropdown.
 *
 * Usage:
 *   const picker = initTimezonePicker('container-id', {
 *       selected: 'Asia/Bangkok',
 *       onChange: (tz) => { ... }
 *   });
 *   picker.value;          // read current value
 *   picker.setValue('UTC'); // set programmatically
 *   picker.destroy();      // cleanup
 */

const browserTz = Intl.DateTimeFormat().resolvedOptions().timeZone;

function getOffsetMinutes(tz) {
    try {
        const now = new Date();
        const fmt = new Intl.DateTimeFormat('en-US', {
            timeZone: tz,
            year: 'numeric', month: 'numeric', day: 'numeric',
            hour: 'numeric', minute: 'numeric', second: 'numeric',
            hour12: false,
        });
        const parts = {};
        for (const p of fmt.formatToParts(now)) {
            parts[p.type] = parseInt(p.value, 10);
        }
        const localAsUtc = Date.UTC(parts.year, parts.month - 1, parts.day,
            parts.hour === 24 ? 0 : parts.hour, parts.minute, parts.second);
        return Math.round((localAsUtc - now.getTime()) / 60000);
    } catch {
        return 0;
    }
}

function formatOffset(mins) {
    const sign = mins >= 0 ? '+' : '-';
    const absMin = Math.abs(mins);
    const h = Math.floor(absMin / 60);
    const m = absMin % 60;
    return `UTC${sign}${h}${m ? ':' + String(m).padStart(2, '0') : ''}`;
}

const allTimezones = (Intl.supportedValuesOf ? Intl.supportedValuesOf('timeZone') : ['UTC']).map(tz => {
    const mins = getOffsetMinutes(tz);
    return { name: tz, offset: formatOffset(mins) };
});

// Module-level registry keyed by container ID - survives innerHTML replacement
const pickerRegistry = new Map();

export function initTimezonePicker(containerId, options = {}) {
    const container = document.getElementById(containerId);
    if (!container) return null;

    // Cleanup previous instance (old DOM node may be gone, but dropdown/listener remain)
    if (pickerRegistry.has(containerId)) {
        pickerRegistry.get(containerId)();
        pickerRegistry.delete(containerId);
    }

    const { selected, onChange } = options;
    let currentValue = selected || browserTz;
    let isOpen = false;
    let highlightIndex = -1;
    let filteredItems = [...allTimezones];

    // Build DOM
    container.classList.add('tz-picker');
    container.innerHTML = '';

    // Hidden input for form submission
    const hiddenInput = document.createElement('input');
    hiddenInput.type = 'hidden';
    hiddenInput.name = container.dataset.name || 'timezone';
    hiddenInput.value = currentValue;
    container.appendChild(hiddenInput);

    // Display button
    const display = document.createElement('button');
    display.type = 'button';
    display.className = 'tz-picker-display';
    container.appendChild(display);

    // Dropdown
    const dropdown = document.createElement('div');
    dropdown.className = 'tz-picker-dropdown';
    dropdown.style.display = 'none';

    const searchInput = document.createElement('input');
    searchInput.type = 'text';
    searchInput.className = 'tz-picker-search';
    searchInput.placeholder = 'Search timezones...';
    searchInput.autocomplete = 'off';
    dropdown.appendChild(searchInput);

    const list = document.createElement('div');
    list.className = 'tz-picker-list';
    list.setAttribute('role', 'listbox');
    dropdown.appendChild(list);

    // Append to body to escape modal overflow clipping
    document.body.appendChild(dropdown);

    // The panel lists "Region/City  UTC+05:30", which needs room the trigger
    // does not: the trigger is sized by whatever zone is currently selected,
    // and inside a compact row that can be narrow enough to clip every name in
    // the list down to "Pac...". The panel is a floating layer, so it can be
    // wider than what opened it.
    const MIN_DROPDOWN_WIDTH = 260;
    const VIEWPORT_MARGIN = 8;

    function positionDropdown() {
        const rect = display.getBoundingClientRect();
        const width = Math.max(rect.width, MIN_DROPDOWN_WIDTH);
        dropdown.style.width = width + 'px';
        // Aligned to the trigger, but never past the right edge - widening it
        // is what makes that reachable.
        const maxLeft = window.innerWidth - width - VIEWPORT_MARGIN;
        dropdown.style.left = Math.max(VIEWPORT_MARGIN, Math.min(rect.left, maxLeft)) + 'px';
        // Show below button, or above if not enough space below
        const spaceBelow = window.innerHeight - rect.bottom;
        const dropdownHeight = 290; // search + list max-height
        if (spaceBelow < dropdownHeight && rect.top > spaceBelow) {
            dropdown.style.top = '';
            dropdown.style.bottom = (window.innerHeight - rect.top + 4) + 'px';
        } else {
            dropdown.style.bottom = '';
            dropdown.style.top = (rect.bottom + 4) + 'px';
        }
    }

    function getTzInfo(tzName) {
        return allTimezones.find(t => t.name === tzName) || { name: tzName, offset: '' };
    }

    function updateDisplay() {
        const info = getTzInfo(currentValue);
        // Named as well as read out. The trigger's text is the zone it is
        // showing, which tells someone what is selected but not what the
        // control is for - and where this sits without a visible label, that
        // is the only thing saying so.
        display.setAttribute('aria-label', `Timezone: ${info.name}`);
        display.innerHTML = `
            <span class="tz-picker-value">${escapeHtml(info.name)}</span>
            <span class="tz-picker-offset">${info.offset}</span>
            <svg class="tz-picker-chevron" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="6 9 12 15 18 9"></polyline></svg>
        `;
    }

    function renderList() {
        list.innerHTML = '';
        if (filteredItems.length === 0) {
            list.innerHTML = '<div class="tz-picker-empty">No timezones found</div>';
            return;
        }
        filteredItems.forEach((tz, i) => {
            const item = document.createElement('div');
            item.className = 'tz-picker-item' +
                (tz.name === currentValue ? ' is-selected' : '') +
                (tz.name === browserTz ? ' is-browser' : '') +
                (i === highlightIndex ? ' is-highlighted' : '');
            item.setAttribute('role', 'option');
            item.dataset.index = i;
            item.innerHTML = `
                <span class="tz-picker-item-name">${escapeHtml(tz.name)}${tz.name === browserTz ? ' <span class="tz-picker-local-badge">local</span>' : ''}</span>
                <span class="tz-picker-item-offset">${tz.offset}</span>
            `;
            item.addEventListener('mousedown', (e) => {
                e.preventDefault(); // prevent blur on searchInput
                selectItem(tz.name);
            });
            item.addEventListener('mouseenter', () => {
                highlightIndex = i;
                updateHighlight();
            });
            list.appendChild(item);
        });
    }

    function updateHighlight() {
        const items = list.querySelectorAll('.tz-picker-item');
        items.forEach((el, i) => {
            el.classList.toggle('is-highlighted', i === highlightIndex);
        });
        // Scroll highlighted item into view
        const highlighted = list.querySelector('.is-highlighted');
        if (highlighted) {
            highlighted.scrollIntoView({ block: 'nearest' });
        }
    }

    function filterList(query) {
        const q = query.toLowerCase().trim();
        if (!q) {
            filteredItems = [...allTimezones];
        } else {
            filteredItems = allTimezones.filter(tz =>
                tz.name.toLowerCase().includes(q) ||
                tz.offset.toLowerCase().includes(q)
            );
        }
        highlightIndex = filteredItems.length > 0 ? 0 : -1;
        renderList();
    }

    function selectItem(tzName) {
        currentValue = tzName;
        hiddenInput.value = tzName;
        updateDisplay();
        close();
        // Fire a native change event on the hidden input
        hiddenInput.dispatchEvent(new Event('change', { bubbles: true }));
        if (onChange) onChange(tzName);
    }

    function open() {
        if (isOpen) return;
        isOpen = true;
        dropdown.style.display = '';
        positionDropdown();
        display.classList.add('is-open');
        searchInput.value = '';
        filterList('');
        searchInput.focus();
        // Scroll to selected item
        requestAnimationFrame(() => {
            const selectedEl = list.querySelector('.is-selected');
            if (selectedEl) selectedEl.scrollIntoView({ block: 'center' });
        });
    }

    function close() {
        if (!isOpen) return;
        isOpen = false;
        dropdown.style.display = 'none';
        display.classList.remove('is-open');
    }

    // Event listeners
    display.addEventListener('click', () => {
        isOpen ? close() : open();
    });

    searchInput.addEventListener('input', () => {
        filterList(searchInput.value);
    });

    searchInput.addEventListener('keydown', (e) => {
        if (e.key === 'ArrowDown') {
            e.preventDefault();
            if (highlightIndex < filteredItems.length - 1) {
                highlightIndex++;
                updateHighlight();
            }
        } else if (e.key === 'ArrowUp') {
            e.preventDefault();
            if (highlightIndex > 0) {
                highlightIndex--;
                updateHighlight();
            }
        } else if (e.key === 'Enter') {
            e.preventDefault();
            if (highlightIndex >= 0 && highlightIndex < filteredItems.length) {
                selectItem(filteredItems[highlightIndex].name);
            }
        } else if (e.key === 'Escape') {
            close();
            display.focus();
        }
    });

    // Close on outside click
    function onDocumentClick(e) {
        if (!container.contains(e.target) && !dropdown.contains(e.target)) {
            close();
        }
    }
    document.addEventListener('click', onDocumentClick);

    // Initialize
    updateDisplay();

    function escapeHtml(str) {
        const div = document.createElement('div');
        div.textContent = str;
        return div.innerHTML;
    }

    function destroy() {
        document.removeEventListener('click', onDocumentClick);
        dropdown.remove();
        pickerRegistry.delete(containerId);
    }

    pickerRegistry.set(containerId, destroy);

    return {
        get value() { return currentValue; },
        setValue(tz) {
            currentValue = tz;
            hiddenInput.value = tz;
            updateDisplay();
        },
        destroy
    };
}
