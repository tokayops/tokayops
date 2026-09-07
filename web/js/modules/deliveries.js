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
    assume_accepted: 'Treat the message as delivered, although the provider did not confirm it.',
    cancel: 'Nothing more is sent. The delivery ends as canceled.',
    retry_current_generation: 'Send again to the same address with the same key.',
    retry_new_generation: 'Send a new message. The old one, if it was sent, stays.',
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
    business_closed: 'The alert is already resolved',
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
    period: '24h',
    from: '',
    to: '',
};

// What the delivery modal is showing, so a decision can return to it.
let openJournalId = null;

// ========================================
// Labels
// ========================================

// The status in words a person reads without the state machine at hand. The
// CSS class keeps the status itself, and so do the filters and the API.
const STATUS_WORDS = {
    pending: 'waiting',
    sending: 'sending',
    idle: 'up to date',
    manual_review: 'needs decision',
    succeeded: 'delivered',
    permanent_failed: 'failed',
    expired: 'expired',
    canceled: 'canceled',
};

export function statusWords(status) {
    return STATUS_WORDS[status] || String(status || '').replace(/_/g, ' ');
}

export function statusBadge(status) {
    return `<span class="delivery-status delivery-status-${escapeHtml(status)}">${escapeHtml(statusWords(status))}</span>`;
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
        case 'thread':
            return `<span class="delivery-target"><i data-lucide="message-square"></i><span>thread in #${id}</span></span>`;
        case 'thread_reply':
            return `<span class="delivery-target"><i data-lucide="corner-down-right"></i><span>reply in #${id}</span></span>`;
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

// ========================================
// The alert group's deliveries
// ========================================


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

/**
 * The journal is one story, told top down: what this delivery is, where it
 * stands and why, then what happened to it in the order it happened. The
 * machinery - generations, revisions, receipts, the events the machine
 * writes for itself - stays behind "Details".
 */
const providerNames = { slack: 'Slack', telegram: 'Telegram', webhook: 'Webhook' };

function providerName(provider) {
    return providerNames[provider] || provider || '';
}

/**
 * One sentence for what the delivery is. The recipient keeps the label
 * helper, so a person's id becomes a name once the directory answers.
 */
function deliverySentence(d) {
    const satellite = d.target_kind === 'thread' || d.target_kind === 'thread_reply';
    const to = targetLabel(satellite ? 'channel' : d.target_kind, d.target_ref);
    const erased = d.recipient_erased ? ' <span class="text-muted">(erased)</span>' : '';
    switch (d.target_kind) {
        case 'thread': return `Thread for the card in ${to}${erased}`;
        case 'thread_reply': return `Reply for the card in ${to}${erased}`;
        case 'subscriber': return `Event to ${to}${erased}`;
        case 'user': return `Message to ${to}${erased}`;
        default: return `${d.form === 'editable' ? 'Card' : 'Message'} in ${to}${erased}`;
    }
}

function humanClass(value) {
    return String(value || '').replace(/_/g, ' ');
}

/**
 * Where the delivery stands, in the interface's words: the status alone is
 * a label, and the operator needs the reason that goes with it.
 */
function verdict(journal) {
    const d = journal.delivery;
    const attempts = journal.attempts || [];
    const last = attempts[attempts.length - 1];
    const said = last && (last.summary || last.result_detail || humanClass(last.error_class) || last.provider_status);
    const events = journal.events || [];
    const lastEvent = events[events.length - 1];
    switch (d.status) {
        case 'permanent_failed':
            return `Failed permanently${said ? `: ${escapeHtml(said)}` : '.'}`;
        case 'manual_review':
            return `The last attempt has no clear result. A person must decide${said ? `: ${escapeHtml(said)}` : '.'}`;
        case 'expired':
            return `Not sent before the deadline${d.expires_at ? ` (${when(d.expires_at)})` : ''}.`;
        case 'canceled':
            return `Canceled${lastEvent && lastEvent.reason ? `: ${escapeHtml(lastEvent.reason)}` : '.'}`;
        case 'sending':
            return 'Sending now.';
        case 'pending':
            if (d.target_kind === 'thread_reply') return 'Will be sent after the alert is resolved and the card is updated.';
            if (d.target_kind === 'thread') return 'Will be sent after the card is posted or updated.';
            return d.attempts_in_generation > 0
                ? `Next attempt at ${when(d.next_attempt_at)}.`
                : `Scheduled for ${when(d.next_attempt_at)}.`;
        case 'idle':
            return 'Delivered. The message is updated when the alert changes.';
        case 'succeeded':
            return d.form === 'editable'
                ? 'Delivered. The alert is resolved and the message shows it.'
                : 'Delivered.';
        default:
            return '';
    }
}

/**
 * What an attempt did, and what came of it, as a person would say it. "Sent"
 * is only said once the provider accepted; a rejected attempt is named as
 * the try it was.
 */
function attemptWords(a, d) {
    const answer = a.summary || a.result_detail || '';
    const said = answer && answer !== 'ok' && answer !== a.provider_status ? answer : '';
    const words = (what, outcome, tone) => ({ what, outcome, tone, said });

    if (a.record_kind !== 'attempt') {
        // The delivery was not attempted: preparation found it could not be.
        if (a.outcome === 'permanent_rejection') return words('Not sent', 'no retry', 'failed');
        if (a.outcome === 'retryable_rejection') return words('Not sent', 'will retry', 'retry');
        return words('Not sent', humanClass(a.outcome), 'doubt');
    }

    const thing = d.form === 'editable' && d.target_kind === 'channel' ? 'Card' : 'Message';
    const mutation = a.attempt_kind === 'mutation';
    const done = !mutation ? 'Sent'
        : a.operation === 'resolve' ? `${thing} marked resolved` : `${thing} updated`;
    const tried = mutation ? `${thing} update` : 'Send';

    switch (a.outcome) {
        case 'accepted': return words(done, 'accepted', 'ok');
        case 'retryable_rejection': return words(`${tried} rejected`, 'will retry', 'retry');
        case 'permanent_rejection': return words(`${tried} rejected`, 'no retry', 'failed');
        case 'ambiguous': return words(mutation ? `${tried} sent` : 'Sent', 'no response', 'doubt');
        case undefined: case null: case '': return words(mutation ? `${tried} in progress` : 'Sending', '', 'open');
        default: return words(tried, humanClass(a.outcome), 'doubt');
    }
}

const machinery = new Set(['effect_bound', 'desired_raised', 'generation_started']);

const eventWords = {
    created: 'Created',
    canceled: 'Canceled',
    cancellation_requested: 'Cancellation requested',
    operator_decision: 'Operator decision',
    revived: 'Restored',
    expired: 'Expired',
    duplicate_risk_accepted: 'Duplicate risk accepted',
    effect_bound: 'Address assigned',
    desired_raised: 'New revision requested',
    generation_started: 'New generation started',
};

/**
 * The journal is drawn with the alert group's own parts: the hero line, the
 * technical details behind a summary, and the timeline - so a person who
 * reads one reads the other.
 */
function relative(ts) {
    const c = window.Components;
    return c && typeof c.timeSince === 'function' ? c.timeSince(ts, { withAgo: true }) : '';
}

function timelineTime(ts) {
    if (!ts) return '—';
    const at = new Date(ts);
    if (Number.isNaN(at.getTime())) return '—';
    const stamp = at.toLocaleString(undefined, {
        month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit', timeZoneName: 'short',
    });
    const ago = relative(ts);
    return ago ? `${stamp} · ${ago}` : stamp;
}

function timelineItem({ classes, icon, at, message, detail, actor, attrs }) {
    return `
        <div class="timeline-event ${classes}"${attrs || ''}>
            <div class="timeline-icon"><i data-lucide="${icon}"></i></div>
            <div class="timeline-content">
                <div class="timeline-time">${timelineTime(at)}</div>
                <div class="timeline-message">${message}</div>
                ${detail ? `<div class="timeline-actor journal-said">${detail}</div>` : ''}
                ${actor ? `<div class="timeline-actor">by ${actor}</div>` : ''}
            </div>
        </div>`;
}

function attemptRow(a, d) {
    const w = attemptWords(a, d);
    const type = { ok: 'type-notification_sent is-key', failed: 'type-notification_failed is-key',
        retry: 'type-journal-retry is-minor', doubt: 'type-journal-retry is-minor', open: 'type-journal-open is-minor' }[w.tone];
    const icon = { ok: 'send', failed: 'x-circle', retry: 'refresh-cw', doubt: 'help-circle', open: 'loader' }[w.tone];
    const erased = a.receipt_redacted_at
        ? ' <span class="journal-note" title="The coordinates were removed by an erasure">receipt erased</span>'
        : '';
    const detail = [w.said ? escapeHtml(w.said) : '', a.error_class ? `<span class="journal-note">${escapeHtml(a.error_class)}</span>` : '']
        .filter(Boolean).join(' ');
    return timelineItem({
        classes: `journal-attempt ${type}`, icon, at: a.started_at || a.finished_at,
        message: `${escapeHtml(w.what)}${w.outcome ? `, <span class="journal-tone journal-tone-${escapeAttr(w.tone)}">${escapeHtml(w.outcome)}</span>` : ''}${erased}`,
        detail, attrs: ` data-outcome="${escapeAttr(a.outcome || '')}"`,
    });
}

const eventLook = {
    created: ['type-created is-key', 'bell-ring'],
    canceled: ['type-journal-withdrawn is-minor', 'ban'],
    cancellation_requested: ['type-journal-withdrawn is-minor', 'ban'],
    operator_decision: ['type-journal-decision is-key', 'gavel'],
    revived: ['type-journal-decision is-key', 'undo-2'],
    expired: ['type-notification_failed is-key', 'clock'],
    lease_lost: ['type-journal-retry is-minor', 'alert-triangle'],
    duplicate_risk_accepted: ['type-journal-decision is-minor', 'copy'],
};

function eventRow(e) {
    const [classes, icon] = eventLook[e.kind] || ['type-journal-machinery is-minor', 'settings-2'];
    return timelineItem({
        classes: `journal-event ${classes}`, icon, at: e.at,
        message: escapeHtml(eventWords[e.kind] || humanClass(e.kind)),
        detail: e.reason ? escapeHtml(e.reason) : '',
        actor: actorLabel(e),
        attrs: ` data-kind="${escapeAttr(e.kind)}"`,
    });
}

/**
 * The attempts and the events that mean something to a person, in the order
 * they happened.
 */
function history(journal) {
    const d = journal.delivery;
    const lines = [];
    (journal.attempts || []).forEach(a => lines.push({ at: a.started_at || a.finished_at, html: attemptRow(a, d) }));
    (journal.events || []).filter(e => !machinery.has(e.kind)).forEach(e => lines.push({ at: e.at, html: eventRow(e) }));
    lines.sort((x, y) => new Date(x.at || 0) - new Date(y.at || 0));
    if (lines.length === 0) return '<div class="journal-none">No activity yet.</div>';
    return `<div class="timeline-container"><div class="timeline journal-history">${lines.map(l => l.html).join('')}</div></div>`;
}

function detailItem(label, value, mono) {
    return `
        <div class="detail-item">
            <div class="detail-label">${escapeHtml(label)}</div>
            <div class="detail-value"${mono ? ' style="font-family: monospace; font-size: 0.8rem;"' : ''}>${value}</div>
        </div>`;
}

function details(journal) {
    const d = journal.delivery;
    const receipt = d.recipient_erased && d.receipt_recorded
        ? 'recorded (coordinates erased)'
        : (d.receipt_recorded ? 'recorded' : 'none');
    const items = [
        detailItem('Generation', escapeHtml(`${d.generation_no}, ${d.attempts_in_generation} ${d.attempts_in_generation === 1 ? 'attempt' : 'attempts'}`)),
        detailItem('Revision', escapeHtml(`desired ${d.desired_revision}, applied ${d.applied_revision ?? '—'}${d.final_revision_applied ? ', final' : ''}`)),
        detailItem('Receipt', escapeHtml(receipt)),
        d.expires_at ? detailItem('Expires', escapeHtml(`${when(d.expires_at)}${d.expired ? ', passed' : ''}`)) : '',
        detailItem('Family', escapeHtml(`${d.family}, ${d.kind}`)),
        d.alert_group_id ? detailItem('Alert group', `<a href="#/ops/alert-groups/${escapeAttr(d.alert_group_id)}" class="journal-group-link">${escapeHtml(d.alert_group_id)}</a>`, true) : '',
        detailItem('Delivery ID', escapeHtml(d.id), true),
    ].filter(Boolean).join('');
    const machineryLines = (journal.events || []).filter(e => machinery.has(e.kind)).map(eventRow).join('');
    const late = journal.observations && journal.observations.length > 0
        ? `<div class="journal-none">${journal.observations.length === 1 ? 'One late response' : `${journal.observations.length} late responses`} arrived after the attempt was closed and ${journal.observations.length === 1 ? 'is' : 'are'} kept with it.</div>`
        : '';
    return `
        <details class="detail-section detail-technical journal-details">
            <summary class="detail-section-title detail-summary">
                <span class="detail-summary-title">Technical details</span>
            </summary>
            <div class="detail-grid">${items}</div>
            <div class="detail-subsection">
                <div class="detail-subtitle">Timestamps</div>
                <div class="detail-grid">
                    ${detailItem('Created', escapeHtml(when(d.created_at)))}
                    ${detailItem('Updated', escapeHtml(when(d.updated_at)))}
                </div>
            </div>
            ${machineryLines ? `
                <div class="detail-subsection">
                    <div class="detail-subtitle">Internal events</div>
                    <div class="timeline journal-machinery">${machineryLines}</div>
                </div>` : ''}
            ${late}
        </details>`;
}

export function journalPanel(journal) {
    const d = journal.delivery;
    const said = verdict(journal);
    const attempts = d.attempts_in_generation;
    return `
        <div class="journal">
            <div class="detail-hero">
                <div class="detail-status-line">
                    <span class="journal-status">${statusBadge(d.status)}</span>
                    <span class="status-sep">·</span>
                    <span class="journal-sentence">${deliverySentence(d)}</span>
                    <span class="status-sep">·</span>
                    <span class="status-time">via ${escapeHtml(providerName(d.provider))}</span>
                </div>
                ${said ? `<div class="journal-verdict">${said}</div>` : ''}
                <div class="detail-meta-row">
                    <span class="detail-meta-chip">${attempts} ${attempts === 1 ? 'attempt' : 'attempts'}</span>
                    <span class="detail-meta-chip">Created ${escapeHtml(relative(d.created_at) || when(d.created_at))}</span>
                    <span class="detail-meta-chip">Updated ${escapeHtml(relative(d.updated_at) || when(d.updated_at))}</span>
                </div>
            </div>
            ${details(journal)}
            <div class="detail-section">
                <h3 class="detail-section-title">History</h3>
                ${history(journal)}
            </div>
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
        Elements.deliveryModalTitle.textContent = 'Delivery';
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
                    <div class="form-hint">To retry an expired delivery, set a new deadline in the future.</div>
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
            showRefusal('invalid_decision', 'The new deadline must be in the future.');
            return;
        }
        body.new_expires_at = at.toISOString();
    }

    submit.disabled = true;
    try {
        const result = await API.deliveries.decide(delivery.id, body);
        showToast(`Decision applied: ${statusWords(result.status)}`, 'success');
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
            showRefusal(outcome, error.body.detail || (error.body.status ? `The delivery is ${statusWords(error.body.status)}.` : ''));
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

const FAMILY_WORDS = { notification: 'Notifications', handoff: 'Handoffs', webhook: 'Webhooks' };
const KIND_WORDS = {
    escalation: 'escalation', escalation_replay: 'escalation replay',
    handoff: 'on-call handoff', webhook_event: 'webhook event',
};
const PERIODS = [['24h', '24h', 1], ['7d', '7d', 7], ['30d', '30d', 30], ['custom', 'Custom', 0]];

function activityFilters() {
    const option = (value, label, selected) => `<option value="${escapeAttr(value)}" ${selected ? 'selected' : ''}>${escapeHtml(label)}</option>`;
    const custom = activity.period === 'custom';
    return `
        <div class="filters-bar activity-filters">
            <div class="filters-row">
                <div class="filter-group">
                    <div class="filter-label">Family</div>
                    <select id="activity-family" class="form-select" aria-label="Family">
                        ${option('', 'All families', activity.family === '')}
                        ${FAMILIES.map(f => option(f, FAMILY_WORDS[f] || f, activity.family === f)).join('')}
                    </select>
                </div>
                <div class="filter-group">
                    <div class="filter-label">Status</div>
                    <select id="activity-status" class="form-select" aria-label="Status">
                        ${option('', 'All statuses', activity.status === '')}
                        ${STATUSES.map(s => option(s, statusWords(s), activity.status === s)).join('')}
                    </select>
                </div>
                <div class="filters-right">
                    <div class="filter-group">
                        <div class="filter-label">Period</div>
                        <div class="scope-tabs-sm" id="activity-period-tabs" role="tablist" aria-label="Period">
                            ${PERIODS.map(([key, label]) => `<button type="button" class="scope-tab-sm${activity.period === key ? ' active' : ''}" data-period="${key}" aria-selected="${activity.period === key}">${label}</button>`).join('')}
                        </div>
                    </div>
                    <div class="filter-group activity-custom" ${custom ? '' : 'hidden'}>
                        <div class="filter-label">From</div>
                        <input type="datetime-local" id="activity-from" class="form-input" value="${escapeAttr(activity.from)}">
                    </div>
                    <div class="filter-group activity-custom" ${custom ? '' : 'hidden'}>
                        <div class="filter-label">To</div>
                        <input type="datetime-local" id="activity-to" class="form-input" value="${escapeAttr(activity.to)}">
                    </div>
                    <button type="button" class="btn btn-secondary btn-sm activity-custom" id="activity-apply" ${custom ? '' : 'hidden'}>Apply</button>
                </div>
            </div>
        </div>`;
}

/** The second line of a row: who carries it and what kind of promise it is. */
function activityKind(d) {
    const kind = KIND_WORDS[d.kind] || humanClass(d.kind);
    if (d.family === 'webhook') return kind;
    return `${providerName(d.provider)} · ${kind}`;
}

function activityRow(d) {
    const group = d.alert_group_id
        ? `<a href="#/ops/alert-groups/${escapeAttr(d.alert_group_id)}" class="activity-group-link" title="Open the alert group"><i data-lucide="bell"></i> Alert group</a>`
        : '<span class="text-muted">—</span>';
    return `
        <div class="alert-group-card activity-row status-${escapeAttr(d.status)}" data-delivery-id="${escapeAttr(d.id)}" data-family="${escapeAttr(d.family)}" data-status="${escapeAttr(d.status)}">
            <div class="activity-when">${escapeHtml(window.Components?.formatDateTime?.(d.created_at) || when(d.created_at))}<div class="activity-sub">${escapeHtml(relative(d.created_at))}</div></div>
            <div class="activity-what"><div class="activity-sentence">${deliverySentence(d)}</div><div class="activity-sub">${escapeHtml(activityKind(d))}</div></div>
            <div>${statusBadge(d.status)}</div>
            <div>${group}</div>
            <span class="journal-link activity-open" data-delivery-id="${escapeAttr(d.id)}" title="Open the journal"><i data-lucide="scroll-text"></i> Journal <i data-lucide="chevron-right"></i></span>
        </div>`;
}

function activityFooter(response) {
    const page = response.page || 1;
    const totalPages = response.total_pages || 1;
    const total = response.total || 0;
    return `
        <div class="activity-footer">
            <div class="activity-summary">
                <span id="activity-total">${total} ${total === 1 ? 'delivery' : 'deliveries'}</span>
                <span class="activity-dot">·</span>
                <span id="activity-period">${escapeHtml(periodLabel(response))}</span>
            </div>
            <div class="activity-pager">
                <button type="button" class="btn btn-sm btn-secondary" id="activity-prev" ${page <= 1 ? 'disabled' : ''}><i data-lucide="chevron-left"></i> Previous</button>
                <span class="page-info" id="activity-page">Page ${page} of ${totalPages}</span>
                <button type="button" class="btn btn-sm btn-secondary" id="activity-next" ${page >= totalPages ? 'disabled' : ''}>Next <i data-lucide="chevron-right"></i></button>
            </div>
        </div>`;
}

function activityTable(response) {
    const deliveries = response.deliveries || [];
    if (deliveries.length === 0) {
        return `
            <div class="empty-state" id="activity-empty">
                <i data-lucide="inbox" class="empty-icon"></i>
                <p>No deliveries in this period.</p>
                <p class="text-muted">Widen the period or clear a filter.</p>
            </div>
            ${activityFooter(response)}`;
    }
    return `
        <div class="activity-table">
            <div class="list-header activity-header">
                <div class="list-header-col">When</div>
                <div class="list-header-col">Delivery</div>
                <div class="list-header-col">Status</div>
                <div class="list-header-col">Alert group</div>
                <div class="list-header-col"></div>
            </div>
            <div class="activity-rows">${deliveries.map(activityRow).join('')}</div>
        </div>
        ${activityFooter(response)}`;
}

/** The window the log is read over, as the request wants it. */
function periodRange() {
    const days = (PERIODS.find(([key]) => key === activity.period) || [])[2];
    if (days) return activity.period === '24h' ? {} : { from: new Date(Date.now() - days * 86400000).toISOString() };
    const range = {};
    if (activity.from) range.from = new Date(activity.from).toISOString();
    if (activity.to) range.to = new Date(activity.to).toISOString();
    return range;
}

function periodLabel(response) {
    switch (activity.period) {
        case '24h': return 'Last 24 hours';
        case '7d': return 'Last 7 days';
        case '30d': return 'Last 30 days';
        default: {
            const from = response.from || activity.from;
            const to = response.to || activity.to;
            return `${from ? when(from) : '…'} – ${to ? when(to) : 'now'}`;
        }
    }
}

async function loadActivity() {
    const list = document.getElementById('activity-list');
    if (!list) return;
    list.innerHTML = '<div class="loading-spinner">Loading...</div>';
    const params = { page: activity.page, limit: 50, family: activity.family, status: activity.status, ...periodRange() };
    try {
        const response = await API.deliveries.list(params);
        list.innerHTML = activityTable(response);
        if (window.lucide) lucide.createIcons();
        bindJournalLinks(list);
        hydrateUserNames(list);
        // The row itself opens the journal; its links keep their own meaning.
        list.querySelectorAll('.activity-row').forEach(row => {
            row.addEventListener('click', (e) => {
                if (e.target.closest('a, button, .journal-link')) return;
                openDeliveryJournal(row.dataset.deliveryId);
            });
        });
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
            <div class="empty-state" id="activity-forbidden">
                <i data-lucide="lock" class="empty-icon"></i>
                <p>The delivery journal is available to administrators.</p>
            </div>`;
        if (window.lucide) lucide.createIcons();
        return;
    }
    view.innerHTML = `${activityFilters()}<div id="activity-list"></div>`;
    if (window.lucide) lucide.createIcons();

    const reload = () => { activity.page = 1; loadActivity(); };
    document.getElementById('activity-family')?.addEventListener('change', (e) => { activity.family = e.target.value; reload(); });
    document.getElementById('activity-status')?.addEventListener('change', (e) => { activity.status = e.target.value; reload(); });
    document.getElementById('activity-apply')?.addEventListener('click', () => {
        activity.from = document.getElementById('activity-from').value;
        activity.to = document.getElementById('activity-to').value;
        reload();
    });
    const tabs = document.getElementById('activity-period-tabs');
    tabs?.addEventListener('click', (e) => {
        const tab = e.target.closest('[data-period]');
        if (!tab) return;
        activity.period = tab.dataset.period;
        tabs.querySelectorAll('[data-period]').forEach(t => {
            const on = t === tab;
            t.classList.toggle('active', on);
            t.setAttribute('aria-selected', String(on));
        });
        const custom = activity.period === 'custom';
        view.querySelectorAll('.activity-custom').forEach(el => { el.hidden = !custom; });
        if (!custom) reload();
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
