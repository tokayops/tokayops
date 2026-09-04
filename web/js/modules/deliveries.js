/**
 * Deliveries: what the system promised to send, and what became of it.
 *
 * Three views over one resource. The block in an alert group's details shows
 * the group's paging and, from the claims on its events, its webhook
 * deliveries - it is read under the group's own permission and names nobody's
 * address. The journal of one delivery - attempts, observations, lifecycle
 * events - is the administrator's, and so is the operational log under
 * Activity. A decision about a stuck delivery is taken from the journal.
 *
 * Who wrote a journal line is read from actor_kind, never from the text of
 * the actor: a person is named through the users directory - which answers
 * "Deleted user" for somebody erased - a component by its label, and a line a
 * build before this one wrote is shown as the text it wrote, marked as such.
 */

import { State } from '/js/core/state.js';
import { Elements, showToast, escapeHtml, escapeAttr, openModal, closeModalById } from '/js/core/utils.js';
import { Permissions } from '/js/modules/permissions.js';
import { resolveNames } from '/js/core/users-directory.js';

export const FAMILIES = ['notification', 'handoff', 'webhook'];
export const STATUSES = [
    'pending', 'sending', 'idle', 'manual_review',
    'succeeded', 'permanent_failed', 'expired', 'canceled',
];

// The decisions a person may take, by the status the delivery is in. The
// matrix is the domain's (D1, T25-T34): a card in review may be assumed
// delivered, withdrawn or retried; a delivery that failed for good is
// withdrawn or retried; an expiry is only ever retried, with a new deadline.
// Everything that depends on the last attempt is judged by the server under
// its lock, and its refusal is shown here in its own words.
const DECISIONS_BY_STATUS = {
    manual_review: ['assume_accepted', 'cancel', 'retry_current_generation', 'retry_new_generation'],
    permanent_failed: ['cancel', 'retry_current_generation', 'retry_new_generation'],
    expired: ['retry_current_generation', 'retry_new_generation'],
};

// A webhook delivery has another door to a new effect: the replay, from the
// subscriber's deliveries, which makes a NEW delivery. A retry of this one
// would be a second live delivery beside it, and the server refuses it for
// every webhook - so it is not offered. What is left is to withdraw, or, in
// review, to assume the call landed; an expired webhook is only ever replayed.
const WEBHOOK_DECISIONS_BY_STATUS = {
    manual_review: ['assume_accepted', 'cancel'],
    permanent_failed: ['cancel'],
    expired: [],
};

const REPLAY_HINT = 'A webhook delivery is sent again by a replay from the subscriber\'s deliveries, which makes a new delivery; this one is not retried.';

/**
 * The decisions offered for a delivery: by its status, and by its family.
 */
export function decisionsFor(delivery) {
    const byStatus = delivery?.family === 'webhook' ? WEBHOOK_DECISIONS_BY_STATUS : DECISIONS_BY_STATUS;
    return byStatus[delivery?.status] || [];
}

/**
 * Whether the status is one a person decides about at all - even when this
 * family offers nothing for it, which the journal then says.
 */
function isDecidableStatus(status) {
    return Boolean(DECISIONS_BY_STATUS[status]);
}

const DECISION_LABELS = {
    assume_accepted: 'Assume it was delivered',
    cancel: 'Cancel the delivery',
    retry_current_generation: 'Retry',
    retry_new_generation: 'Retry as a new message',
};

const DECISION_HINTS = {
    assume_accepted: 'The message reached its recipient even though the provider never confirmed it.',
    cancel: 'Nothing more is sent. The delivery ends as canceled.',
    retry_current_generation: 'Send again with the same address and the same key.',
    retry_new_generation: 'Start over: a new message, which may exist beside the old one.',
};

// The decisions that may create a second message, for which the person can
// accept that risk on the record. The box is always shown for them; whether
// it is required is the server's to say.
const DUPLICATE_RISK_DECISIONS = new Set(['assume_accepted', 'retry_new_generation']);

const COMPONENT_LABELS = {
    engine: 'Escalation engine',
    notifier: 'Handoff notifier',
    fanout: 'Webhook fan-out',
    worker: 'Delivery worker',
    recovery: 'Lease recovery',
    erasure: 'Erasure',
    system: 'Alert ingestion',
};

const OUTCOME_LABELS = {
    resolved: 'Applied',
    already_resolved: 'Already decided',
    invalid_decision: 'Refused',
    business_closed: 'The alert is over',
    recipient_erased: 'The recipient was erased',
    not_found: 'Not found',
};

const REASON_LIMIT = 500;

// Deliveries listed by the operational log page: the ones this instance of
// the page is showing, and the filters that produced them.
const activity = {
    page: 1,
    family: '',
    status: '',
    from: '',
    to: '',
};

// What the delivery modal is showing, so a decision can return to it.
let openJournalId = null;

// ========================================
// Labels
// ========================================

export function statusBadge(status) {
    const label = String(status || '').replace(/_/g, ' ');
    return `<span class="delivery-status delivery-status-${escapeHtml(status)}">${escapeHtml(label)}</span>`;
}

/**
 * Where a delivery goes. A person is shown by id until the directory answers
 * with a name (see hydrateUserNames); a channel by its id; a subscriber by
 * the id of the integration.
 */
export function targetLabel(kind, ref) {
    const id = escapeHtml(ref || '');
    switch (kind) {
        case 'user':
            return `<span class="delivery-target" data-user-id="${escapeAttr(ref || '')}"><i data-lucide="user"></i><span class="delivery-target-name">${id}</span></span>`;
        case 'channel':
            return `<span class="delivery-target"><i data-lucide="hash"></i><span>${id}</span></span>`;
        case 'subscriber':
            return `<span class="delivery-target"><i data-lucide="webhook"></i><span>subscriber ${id}</span></span>`;
        default:
            return `<span class="delivery-target">${escapeHtml(kind || '')} ${id}</span>`;
    }
}

/**
 * Who wrote a journal line, by the kind the row carries.
 */
export function actorLabel(event) {
    const ref = escapeHtml(event.actor || '');
    switch (event.actor_kind) {
        case 'user':
            return `<span class="journal-actor journal-actor-user" data-user-id="${escapeAttr(event.actor || '')}"><span class="delivery-target-name">${ref}</span></span>`;
        case 'system':
            return `<span class="journal-actor journal-actor-system">${escapeHtml(COMPONENT_LABELS[event.actor] || event.actor || 'system')}</span>`;
        case 'legacy':
            return `<span class="journal-actor journal-actor-legacy" title="Written by a build before this one">${ref || '—'} <span class="journal-actor-tag">legacy</span></span>`;
        default:
            return `<span class="journal-actor">${ref || '—'}</span>`;
    }
}

/**
 * Turn the user ids a render left behind into names. An erased person comes
 * back from the directory under the name erasure left them - "Deleted user" -
 * and is shown as that. An id the directory does not know at all is a
 * reference nothing can explain: it stays an id, marked as unknown, rather
 * than being dressed up as a person.
 */
export async function hydrateUserNames(root) {
    if (!root) return;
    const holders = Array.from(root.querySelectorAll('[data-user-id]'));
    if (holders.length === 0) return;
    const ids = holders.map(el => el.dataset.userId);
    let names = new Map();
    try {
        names = await resolveNames(ids);
    } catch (error) {
        console.warn('Failed to resolve delivery actors', error);
        return;
    }
    for (const el of holders) {
        const name = names.get(el.dataset.userId);
        const slot = el.querySelector('.delivery-target-name') || el;
        if (name) {
            slot.textContent = name;
        } else {
            el.classList.add('is-unknown');
            el.title = 'No user with this id';
        }
    }
}

function when(value) {
    if (!value) return '—';
    const at = new Date(value);
    if (Number.isNaN(at.getTime())) return '—';
    return at.toLocaleString(undefined, {
        month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit',
    });
}

function canReadJournal() {
    return Permissions.isAdmin();
}

function journalButton(deliveryId) {
    if (!canReadJournal()) return '';
    return `<button type="button" class="btn btn-sm btn-secondary journal-link" data-delivery-id="${escapeAttr(deliveryId)}" title="Open the journal">
        <i data-lucide="scroll-text"></i> Journal
    </button>`;
}

// ========================================
// The alert group's deliveries
// ========================================

function pagingTable(paging) {
    if (!paging || paging.length === 0) {
        return '<div class="deliveries-empty">Nobody was paged for this alert.</div>';
    }
    const rows = paging.map(d => `
        <tr class="delivery-row" data-delivery-id="${escapeAttr(d.id)}">
            <td>${statusBadge(d.status)}</td>
            <td>${escapeHtml(d.provider)}</td>
            <td>${targetLabel(d.target_kind, d.target_ref)}</td>
            <td>${escapeHtml(d.form === 'editable' ? 'card' : 'message')}</td>
            <td>${when(d.created_at)}</td>
            <td class="delivery-row-actions">${journalButton(d.id)}</td>
        </tr>`).join('');
    return `
        <table class="delivery-table deliveries-paging">
            <thead><tr><th>Status</th><th>Provider</th><th>To</th><th>Form</th><th>Created</th><th></th></tr></thead>
            <tbody>${rows}</tbody>
        </table>`;
}

function batchLabel(batch) {
    if (batch.outcome === 'no_targets') return 'Nobody subscribed';
    return batch.kind === 'webhook_replay' ? 'Replay' : 'Fan-out';
}

function eventsList(events) {
    if (!events || events.length === 0) {
        return '<div class="deliveries-empty">No webhook events for this alert.</div>';
    }
    return events.map(event => {
        const batches = (event.batches || []).map(batch => {
            const deliveries = (batch.deliveries || []).map(d => `
                <tr class="delivery-row" data-delivery-id="${escapeAttr(d.id)}">
                    <td>${statusBadge(d.status)}</td>
                    <td>${targetLabel(d.target_kind, d.target_ref)}</td>
                    <td>${when(d.created_at)}</td>
                    <td class="delivery-row-actions">${journalButton(d.id)}</td>
                </tr>`).join('');
            return `
                <div class="delivery-batch" data-batch-kind="${escapeAttr(batch.kind)}" data-batch-outcome="${escapeAttr(batch.outcome)}">
                    <div class="delivery-batch-header">
                        <span class="delivery-batch-kind">${escapeHtml(batchLabel(batch))}</span>
                        <span class="text-muted">${batch.intent_count} ${batch.intent_count === 1 ? 'delivery' : 'deliveries'} · ${when(batch.admitted_at)}</span>
                    </div>
                    ${deliveries ? `<table class="delivery-table deliveries-webhook"><tbody>${deliveries}</tbody></table>` : ''}
                </div>`;
        }).join('');
        const pending = !event.batches || event.batches.length === 0;
        return `
            <div class="delivery-event" data-event-id="${escapeAttr(event.event_id)}" data-event-status="${escapeAttr(event.status)}">
                <div class="delivery-event-header">
                    <span class="delivery-event-type">${escapeHtml(event.event_type)}</span>
                    <span class="delivery-event-status">${escapeHtml(pending ? 'not fanned out yet' : event.status)}</span>
                    <span class="text-muted">${when(event.created_at)}</span>
                </div>
                ${batches}
            </div>`;
    }).join('');
}

export function groupDeliveriesBlock(data) {
    return `
        <div class="deliveries-block">
            <div class="detail-subtitle">Paging</div>
            ${pagingTable(data.paging)}
            <div class="detail-subtitle">Webhooks</div>
            ${eventsList(data.events)}
        </div>`;
}

/**
 * Load the deliveries of an alert group into its details.
 */
export async function renderGroupDeliveries(alertGroupId) {
    const container = document.getElementById('alert-group-deliveries');
    if (!container) return;
    try {
        const data = await API.alertGroups.deliveries(alertGroupId);
        container.innerHTML = groupDeliveriesBlock(data || {});
        if (window.lucide) lucide.createIcons();
        bindJournalLinks(container);
        hydrateUserNames(container);
    } catch (error) {
        container.innerHTML = `<div class="deliveries-empty">Failed to load deliveries: ${escapeHtml(error.message)}</div>`;
    }
}

/**
 * After a timeline render: the names of the people it names, and the links
 * into the journal for those who may read it.
 */
export function afterTimelineRender(container) {
    if (!container) return;
    bindJournalLinks(container);
    hydrateUserNames(container);
}

export function bindJournalLinks(root) {
    if (!root) return;
    root.querySelectorAll('.journal-link').forEach(btn => {
        btn.addEventListener('click', (e) => {
            e.stopPropagation();
            openDeliveryJournal(btn.dataset.deliveryId);
        });
    });
}

// ========================================
// The journal of one delivery
// ========================================

function attemptsTable(attempts) {
    if (!attempts || attempts.length === 0) {
        return '<div class="deliveries-empty">No attempts yet.</div>';
    }
    const rows = attempts.map(a => {
        const receipt = a.receipt_redacted_at
            ? '<span class="text-muted" title="The coordinates were removed by an erasure">redacted</span>'
            : (a.receipt_recorded ? 'recorded' : '—');
        return `
            <tr class="journal-attempt" data-outcome="${escapeAttr(a.outcome || '')}">
                <td>${a.attempt_no}</td>
                <td>${escapeHtml(a.record_kind)}${a.record_kind !== 'attempt' ? '' : ` · ${escapeHtml(a.attempt_kind)}`}</td>
                <td>${escapeHtml(a.outcome || '—')}${a.error_class ? `<div class="text-muted">${escapeHtml(a.error_class)}</div>` : ''}</td>
                <td>${escapeHtml(a.summary || a.provider_status || '')}</td>
                <td>${receipt}</td>
                <td>${when(a.started_at || a.finished_at)}</td>
            </tr>`;
    }).join('');
    return `
        <table class="delivery-table journal-attempts">
            <thead><tr><th>#</th><th>Kind</th><th>Outcome</th><th>Summary</th><th>Receipt</th><th>When</th></tr></thead>
            <tbody>${rows}</tbody>
        </table>`;
}

function eventsTable(events) {
    if (!events || events.length === 0) {
        return '<div class="deliveries-empty">No events.</div>';
    }
    const rows = events.map(e => `
        <tr class="journal-event" data-kind="${escapeAttr(e.kind)}">
            <td>${e.seq}</td>
            <td><strong>${escapeHtml(e.kind.replace(/_/g, ' '))}</strong>${e.reason ? `<div class="journal-reason">${escapeHtml(e.reason)}</div>` : ''}</td>
            <td>${actorLabel(e)}</td>
            <td>${e.from_status || e.to_status ? `${escapeHtml(e.from_status || '·')} → ${escapeHtml(e.to_status || '·')}` : ''}</td>
            <td>${when(e.at)}</td>
        </tr>`).join('');
    return `
        <table class="delivery-table journal-events">
            <thead><tr><th>#</th><th>Event</th><th>By</th><th>Status</th><th>When</th></tr></thead>
            <tbody>${rows}</tbody>
        </table>`;
}

export function journalPanel(journal) {
    const d = journal.delivery;
    const receipt = d.recipient_erased && d.receipt_recorded
        ? 'recorded, coordinates erased'
        : (d.receipt_recorded ? 'recorded' : 'none');
    return `
        <div class="journal">
            <div class="journal-summary">
                <div class="journal-status">${statusBadge(d.status)}</div>
                <dl class="delivery-detail-meta">
                    <dt>Family</dt><dd>${escapeHtml(d.family)} · ${escapeHtml(d.kind)}</dd>
                    <dt>Provider</dt><dd>${escapeHtml(d.provider)}</dd>
                    <dt>To</dt><dd>${targetLabel(d.target_kind, d.target_ref)}${d.recipient_erased ? ' <span class="text-muted">(erased)</span>' : ''}</dd>
                    <dt>Form</dt><dd>${escapeHtml(d.form === 'editable' ? 'card' : 'message')}</dd>
                    <dt>Generation</dt><dd>${d.generation_no} · ${d.attempts_in_generation} attempt(s)</dd>
                    <dt>Revision</dt><dd>desired ${d.desired_revision}, applied ${d.applied_revision ?? '—'}${d.final_revision_applied ? ' (final)' : ''}</dd>
                    <dt>Receipt</dt><dd>${receipt}</dd>
                    <dt>Created</dt><dd>${when(d.created_at)}</dd>
                    <dt>Updated</dt><dd>${when(d.updated_at)}</dd>
                    ${d.expires_at ? `<dt>Expires</dt><dd>${when(d.expires_at)}${d.expired ? ' (passed)' : ''}</dd>` : ''}
                    ${d.alert_group_id ? `<dt>Alert group</dt><dd><a href="#/ops/alert-groups/${escapeAttr(d.alert_group_id)}" class="journal-group-link">${escapeHtml(d.alert_group_id)}</a></dd>` : ''}
                    <dt>Delivery id</dt><dd class="journal-id">${escapeHtml(d.id)}</dd>
                </dl>
            </div>
            <div class="detail-subtitle">Attempts</div>
            ${attemptsTable(journal.attempts)}
            ${journal.observations && journal.observations.length > 0 ? `
                <div class="detail-subtitle">Late results</div>
                <div class="deliveries-empty">${journal.observations.length} result(s) arrived after the attempt was closed.</div>` : ''}
            <div class="detail-subtitle">Events</div>
            ${eventsTable(journal.events)}
        </div>`;
}

export function canDecide(delivery) {
    return canReadJournal() && decisionsFor(delivery).length > 0;
}

/**
 * Open the journal of one delivery in the delivery modal.
 */
export async function openDeliveryJournal(deliveryId) {
    openJournalId = deliveryId;
    Elements.deliveryModalTitle.textContent = 'Delivery';
    Elements.deliveryModalBody.innerHTML = '<div class="loading-spinner">Loading...</div>';
    Elements.deliveryModalFooter.innerHTML = '';
    openModal('delivery-modal-overlay');

    try {
        const journal = await API.deliveries.get(deliveryId);
        if (openJournalId !== deliveryId) return;
        Elements.deliveryModalTitle.textContent = `Delivery · ${journal.delivery.status.replace(/_/g, ' ')}`;
        // A status a person decides about, in a family that offers nothing
        // for it, is told where its door is instead of a button.
        const replayOnly = canReadJournal() && isDecidableStatus(journal.delivery.status) && !canDecide(journal.delivery);
        Elements.deliveryModalBody.innerHTML = journalPanel(journal) + (replayOnly
            ? `<div class="deliveries-empty journal-replay-hint" id="journal-replay-hint">${escapeHtml(REPLAY_HINT)}</div>`
            : '');
        Elements.deliveryModalFooter.innerHTML = `
            <div class="modal-footer-right">
                ${canDecide(journal.delivery) ? `
                    <button type="button" class="btn btn-primary" id="delivery-decide-btn">
                        <i data-lucide="gavel"></i> Decide
                    </button>` : ''}
                <button type="button" class="btn btn-secondary" id="delivery-modal-close-btn">Close</button>
            </div>`;
        if (window.lucide) lucide.createIcons();
        hydrateUserNames(Elements.deliveryModalBody);
        document.getElementById('delivery-modal-close-btn')?.addEventListener('click', closeDeliveryModal);
        document.getElementById('delivery-decide-btn')?.addEventListener('click', () => openDecision(journal));
    } catch (error) {
        const message = error.status === 404 ? journalMissingMessage(error.body) : error.message;
        Elements.deliveryModalBody.innerHTML = `<div class="empty-state"><p>${escapeHtml(message)}</p></div>`;
        Elements.deliveryModalFooter.innerHTML = `<div class="modal-footer-right"><button type="button" class="btn btn-secondary" id="delivery-modal-close-btn">Close</button></div>`;
        document.getElementById('delivery-modal-close-btn')?.addEventListener('click', closeDeliveryModal);
    }
}

/**
 * What a journal that is not there says: whether history has a term, and
 * how long it is, is in the answer.
 */
export function journalMissingMessage(body) {
    const days = Number(body?.retention_days);
    if (Number.isFinite(days) && days > 0) {
        return `This delivery is not in the journal. Delivery history is kept for ${days} day${days === 1 ? '' : 's'}.`;
    }
    return 'This delivery is not in the journal.';
}

export function closeDeliveryModal() {
    openJournalId = null;
    closeModalById('delivery-modal-overlay');
}

// ========================================
// The operator's decision
// ========================================

function localDateTimeValue(date) {
    const pad = (n) => String(n).padStart(2, '0');
    return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

export function decisionForm(delivery) {
    const decisions = decisionsFor(delivery);
    const options = decisions.map((decision, i) => `
        <label class="decision-option">
            <input type="radio" name="decision" value="${decision}" ${i === 0 ? 'checked' : ''}>
            <span class="decision-option-body">
                <span class="decision-option-label">${escapeHtml(DECISION_LABELS[decision])}</span>
                <span class="decision-option-hint">${escapeHtml(DECISION_HINTS[decision])}</span>
            </span>
        </label>`).join('');
    const needsDeadline = delivery.status === 'expired';
    const inAnHour = new Date(Date.now() + 60 * 60 * 1000);
    return `
        <form id="decision-form" class="decision-form" data-delivery-id="${escapeAttr(delivery.id)}">
            <div class="decision-context">
                ${statusBadge(delivery.status)}
                <span>${escapeHtml(delivery.provider)} · </span>${targetLabel(delivery.target_kind, delivery.target_ref)}
            </div>
            ${delivery.family === 'webhook' ? `<div class="form-hint decision-replay-hint">${escapeHtml(REPLAY_HINT)}</div>` : ''}
            <fieldset class="decision-options">
                <legend>Decision</legend>
                ${options}
            </fieldset>
            <div class="form-group decision-risk" style="${DUPLICATE_RISK_DECISIONS.has(decisions[0]) ? '' : 'display: none;'}">
                <label class="checkbox-label">
                    <input type="checkbox" name="accepted_duplicate_risk" id="decision-risk">
                    <span>I accept that a second message may already exist</span>
                </label>
            </div>
            ${needsDeadline ? `
                <div class="form-group">
                    <label for="decision-deadline">New deadline</label>
                    <input type="datetime-local" id="decision-deadline" name="new_expires_at" value="${localDateTimeValue(inAnHour)}" required>
                    <div class="form-hint">An expired delivery is only retried with a deadline ahead of now.</div>
                </div>` : ''}
            <div class="form-group">
                <label for="decision-reason">Reason</label>
                <textarea id="decision-reason" name="reason" rows="3" maxlength="${REASON_LIMIT}" placeholder="Why - this goes into the journal"></textarea>
                <div class="form-hint"><span id="decision-reason-count">0</span> / ${REASON_LIMIT}</div>
            </div>
            <div class="decision-refusal" id="decision-refusal" role="alert" hidden></div>
        </form>`;
}

function openDecision(journal) {
    const delivery = journal.delivery;
    Elements.deliveryModalTitle.textContent = 'Decide';
    Elements.deliveryModalBody.innerHTML = decisionForm(delivery);
    Elements.deliveryModalFooter.innerHTML = `
        <div class="modal-footer-left">
            <button type="button" class="btn btn-secondary" id="decision-back-btn">
                <i data-lucide="arrow-left"></i> Back
            </button>
        </div>
        <div class="modal-footer-right">
            <button type="submit" form="decision-form" class="btn btn-primary" id="decision-submit-btn">Apply</button>
        </div>`;
    Elements.deliveryModalFooter.classList.add('split');
    if (window.lucide) lucide.createIcons();
    hydrateUserNames(Elements.deliveryModalBody);

    const form = document.getElementById('decision-form');
    const risk = form.querySelector('.decision-risk');
    form.querySelectorAll('input[name="decision"]').forEach(input => {
        input.addEventListener('change', () => {
            risk.style.display = DUPLICATE_RISK_DECISIONS.has(input.value) ? '' : 'none';
            hideRefusal();
        });
    });
    const reason = document.getElementById('decision-reason');
    const count = document.getElementById('decision-reason-count');
    reason.addEventListener('input', () => {
        count.textContent = String(Array.from(reason.value).length);
    });
    document.getElementById('decision-back-btn')?.addEventListener('click', () => {
        Elements.deliveryModalFooter.classList.remove('split');
        openDeliveryJournal(delivery.id);
    });
    form.addEventListener('submit', (e) => {
        e.preventDefault();
        submitDecision(delivery);
    });
}

function hideRefusal() {
    const box = document.getElementById('decision-refusal');
    if (box) {
        box.hidden = true;
        box.innerHTML = '';
    }
}

function showRefusal(outcome, detail) {
    const box = document.getElementById('decision-refusal');
    if (!box) return;
    box.hidden = false;
    box.innerHTML = `
        <div class="decision-refusal-outcome">${escapeHtml(OUTCOME_LABELS[outcome] || outcome || 'Refused')}</div>
        ${detail ? `<div class="decision-refusal-detail">${escapeHtml(detail)}</div>` : ''}`;
}

async function submitDecision(delivery) {
    const form = document.getElementById('decision-form');
    const submit = document.getElementById('decision-submit-btn');
    const decision = form.querySelector('input[name="decision"]:checked')?.value;
    const reason = form.reason.value.trim();
    const length = Array.from(reason).length;
    hideRefusal();
    if (!decision) {
        showRefusal('invalid_decision', 'Choose a decision.');
        return;
    }
    if (length === 0 || length > REASON_LIMIT) {
        showRefusal('invalid_decision', `A reason is required, at most ${REASON_LIMIT} characters.`);
        return;
    }
    const body = { decision, reason };
    if (DUPLICATE_RISK_DECISIONS.has(decision) && form.accepted_duplicate_risk?.checked) {
        body.accepted_duplicate_risk = true;
    }
    if (form.new_expires_at) {
        const at = new Date(form.new_expires_at.value);
        if (Number.isNaN(at.getTime()) || at.getTime() <= Date.now()) {
            showRefusal('invalid_decision', 'The new deadline has to be ahead of now.');
            return;
        }
        body.new_expires_at = at.toISOString();
    }

    submit.disabled = true;
    try {
        const result = await API.deliveries.decide(delivery.id, body);
        showToast(`Decision applied: ${result.status.replace(/_/g, ' ')}`, 'success');
        Elements.deliveryModalFooter.classList.remove('split');
        document.dispatchEvent(new CustomEvent('tokay:delivery-decided', {
            detail: { id: delivery.id, alertGroupId: delivery.alert_group_id, status: result.status },
        }));
        await openDeliveryJournal(delivery.id);
    } catch (error) {
        // A refusal is an answer with an outcome and, for the guards, the words
        // of the guard. Anything else is an error.
        const outcome = error.body?.outcome;
        if (outcome) {
            showRefusal(outcome, error.body.detail || (error.body.status ? `The delivery is ${error.body.status.replace(/_/g, ' ')}.` : ''));
        } else {
            showRefusal('', error.message);
        }
    } finally {
        submit.disabled = false;
    }
}

// ========================================
// The operational log
// ========================================

function activityFilters() {
    const option = (value, label, selected) => `<option value="${escapeAttr(value)}" ${selected ? 'selected' : ''}>${escapeHtml(label)}</option>`;
    return `
        <div class="activity-filters">
            <label>Family
                <select id="activity-family">
                    ${option('', 'All families', activity.family === '')}
                    ${FAMILIES.map(f => option(f, f, activity.family === f)).join('')}
                </select>
            </label>
            <label>Status
                <select id="activity-status">
                    ${option('', 'All statuses', activity.status === '')}
                    ${STATUSES.map(s => option(s, s.replace(/_/g, ' '), activity.status === s)).join('')}
                </select>
            </label>
            <label>From
                <input type="datetime-local" id="activity-from" value="${escapeAttr(activity.from)}">
            </label>
            <label>To
                <input type="datetime-local" id="activity-to" value="${escapeAttr(activity.to)}">
            </label>
            <button type="button" class="btn btn-secondary btn-sm" id="activity-apply">Apply</button>
            <span class="activity-period text-muted" id="activity-period"></span>
        </div>`;
}

function activityTable(response) {
    const deliveries = response.deliveries || [];
    if (deliveries.length === 0) {
        return '<div class="empty-state" id="activity-empty"><i data-lucide="inbox" class="empty-icon"></i><p>No deliveries in this period.</p></div>';
    }
    const rows = deliveries.map(d => `
        <tr class="delivery-row activity-row" data-delivery-id="${escapeAttr(d.id)}" data-family="${escapeAttr(d.family)}" data-status="${escapeAttr(d.status)}">
            <td>${when(d.created_at)}</td>
            <td>${escapeHtml(d.family)}<div class="text-muted">${escapeHtml(d.kind)}</div></td>
            <td>${escapeHtml(d.provider)}</td>
            <td>${targetLabel(d.target_kind, d.target_ref)}</td>
            <td>${statusBadge(d.status)}</td>
            <td>${d.alert_group_id ? `<a href="#/ops/alert-groups/${escapeAttr(d.alert_group_id)}" class="activity-group-link" title="${escapeAttr(d.alert_group_id)}">alert</a>` : '<span class="text-muted">—</span>'}</td>
            <td class="delivery-row-actions">${journalButton(d.id)}</td>
        </tr>`).join('');
    const page = response.page || 1;
    const totalPages = response.total_pages || 1;
    return `
        <table class="delivery-table activity-table">
            <thead><tr><th>Created</th><th>Family</th><th>Provider</th><th>To</th><th>Status</th><th>Alert</th><th></th></tr></thead>
            <tbody>${rows}</tbody>
        </table>
        <div class="activity-pagination">
            <span id="activity-total">${response.total} deliveries</span>
            <div>
                <button type="button" class="btn btn-sm btn-secondary" id="activity-prev" ${page <= 1 ? 'disabled' : ''}>Prev</button>
                <span id="activity-page">Page ${page} / ${totalPages}</span>
                <button type="button" class="btn btn-sm btn-secondary" id="activity-next" ${page >= totalPages ? 'disabled' : ''}>Next</button>
            </div>
        </div>`;
}

function periodLabel(response) {
    const from = response.from ? new Date(response.from) : null;
    const to = response.to ? new Date(response.to) : null;
    if (!activity.from && !activity.to) return 'Last 24 hours';
    return `${from ? when(from) : '…'} – ${to ? when(to) : 'now'}`;
}

async function loadActivity() {
    const list = document.getElementById('activity-list');
    if (!list) return;
    list.innerHTML = '<div class="loading-spinner">Loading...</div>';
    const params = { page: activity.page, limit: 50, family: activity.family, status: activity.status };
    if (activity.from) params.from = new Date(activity.from).toISOString();
    if (activity.to) params.to = new Date(activity.to).toISOString();
    try {
        const response = await API.deliveries.list(params);
        list.innerHTML = activityTable(response);
        const period = document.getElementById('activity-period');
        if (period) period.textContent = periodLabel(response);
        if (window.lucide) lucide.createIcons();
        bindJournalLinks(list);
        hydrateUserNames(list);
        document.getElementById('activity-prev')?.addEventListener('click', () => { activity.page -= 1; loadActivity(); });
        document.getElementById('activity-next')?.addEventListener('click', () => { activity.page += 1; loadActivity(); });
    } catch (error) {
        list.innerHTML = `<div class="empty-state"><p>Failed to load the journal: ${escapeHtml(error.message)}</p></div>`;
    }
}

/**
 * The operational log under #/ops/activity: every delivery of every family,
 * newest first, over the last day unless the filters say otherwise.
 */
export function showActivityView() {
    const view = Elements.opsActivityView;
    if (!view) return;
    if (!canReadJournal()) {
        view.innerHTML = `
            <div class="section-header">
                <h2 class="section-title"><i data-lucide="activity"></i> Activity</h2>
            </div>
            <div class="empty-state" id="activity-forbidden">
                <i data-lucide="lock" class="empty-icon"></i>
                <p>The delivery journal is available to administrators.</p>
            </div>`;
        if (window.lucide) lucide.createIcons();
        return;
    }
    view.innerHTML = `
        <div class="section-header">
            <h2 class="section-title"><i data-lucide="activity"></i> Activity</h2>
        </div>
        ${activityFilters()}
        <div id="activity-list"></div>`;
    if (window.lucide) lucide.createIcons();
    document.getElementById('activity-apply')?.addEventListener('click', () => {
        activity.family = document.getElementById('activity-family').value;
        activity.status = document.getElementById('activity-status').value;
        activity.from = document.getElementById('activity-from').value;
        activity.to = document.getElementById('activity-to').value;
        activity.page = 1;
        loadActivity();
    });
    ['activity-family', 'activity-status'].forEach(id => {
        document.getElementById(id)?.addEventListener('change', () => {
            document.getElementById('activity-apply')?.click();
        });
    });
    loadActivity();
}

/**
 * Bind what the module owns for the life of the page.
 */
export function bindDeliveriesEvents() {
    Elements.deliveryModalClose?.addEventListener('click', closeDeliveryModal);
    Elements.deliveryModalOverlay?.addEventListener('click', (e) => {
        if (e.target === Elements.deliveryModalOverlay) closeDeliveryModal();
    });
    // The journal modal sits above the alert group's: a link into a group from
    // the journal closes the journal first.
    Elements.deliveryModalBody?.addEventListener('click', (e) => {
        if (e.target.closest('.journal-group-link')) closeDeliveryModal();
    });
}

export const _internal = { DECISIONS_BY_STATUS, WEBHOOK_DECISIONS_BY_STATUS, DECISION_LABELS, COMPONENT_LABELS, activity };
