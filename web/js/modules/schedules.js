/**
 * The schedules feature, wired together.
 *
 * This file names the parts and connects them, and does nothing else: no
 * requests, no markup, no state that outlives a call. The flows cross - the
 * calendar opens an override, leaving an override returns to the calendar,
 * saving anything redraws the widgets - and someone has to own that crossing.
 * When nobody does, the modules reach for each other, the graph turns into a
 * cycle, and the single large file that was split comes back under a new name.
 *
 * So: the feature modules know their own screen and take callbacks for
 * everything beyond it. Only this file imports them, and only this file knows
 * that they are four.
 *
 *   schedule-overview.js    the widgets on the page
 *   schedule-editor.js      the configuration form and its preview
 *   schedule-overrides.js   the override modal, and removing an override
 *   schedule-calendar.js    the calendar and the menu on an override
 *   schedule-components.js  the markup all of them render
 *   schedule-shared.js      values and transformations they agree on
 */

import {
    loadOnCallOverviewRow,
    loadOnCallOverviewRows,
    loadOnCallWidget,
    refreshOnCallUI,
} from '/js/modules/schedule-overview.js';
import { openScheduleEditor } from '/js/modules/schedule-editor.js';
import {
    openOverrideModal,
    removeOverride,
    removeOverrideFromWidget,
} from '/js/modules/schedule-overrides.js';
import { openScheduleCalendar } from '/js/modules/schedule-calendar.js';

// The widgets, for the pages that place them.
export { loadOnCallWidget, loadOnCallOverviewRow, loadOnCallOverviewRows };

// The one piece of schedule markup drawn from outside the feature: the header
// of the on-call list, whose rows are filled in by loadOnCallOverviewRows.
export { onCallListHeader } from '/js/modules/schedule-components.js';

// ========================================
// Wiring
// ========================================

/** The calendar, and everywhere it can lead. */
function openCalendar(teamId) {
    return openScheduleCalendar(teamId, {
        onEditOverride: (overrideId) => openOverrides(teamId, {
            editOverrideId: overrideId,
            returnTo: () => openCalendar(teamId),
        }),
        onRemoveOverride: (overrideId, handlers) =>
            removeOverride({ teamId, overrideId }, handlers),
        onChanged: refreshOnCallUI,
    });
}

function openOverrides(teamId, options = {}) {
    return openOverrideModal(teamId, { ...options, onChanged: refreshOnCallUI });
}

function openEditor(teamId) {
    return openScheduleEditor(teamId, { onChanged: refreshOnCallUI });
}

/**
 * The one listener that outlives every modal.
 *
 * It is delegated because the buttons it answers for are drawn and redrawn by
 * the widgets, and it is bound once for the life of the page because a modal
 * closing must never take it down with it: that is how the page ends up with
 * buttons that look right and do nothing.
 */
export function bindScheduleEvents() {
    document.addEventListener('click', (e) => {
        const editBtn = e.target.closest('.edit-schedule-btn');
        if (editBtn?.dataset.teamId) {
            openEditor(editBtn.dataset.teamId);
            return;
        }

        const overrideBtn = e.target.closest('.create-override-btn');
        if (overrideBtn?.dataset.teamId) {
            openOverrides(overrideBtn.dataset.teamId);
            return;
        }

        const viewBtn = e.target.closest('.view-schedule-btn');
        if (viewBtn?.dataset.teamId) {
            openCalendar(viewBtn.dataset.teamId);
            return;
        }

        // The X on an override in an on-call widget. The same button inside
        // the override modal belongs to that modal, which stops its clicks
        // before they reach here.
        const deleteBtn = e.target.closest('.delete-override-btn');
        if (deleteBtn) {
            removeOverrideFromWidget(deleteBtn, { onRemoved: refreshOnCallUI });
        }
    });
}
