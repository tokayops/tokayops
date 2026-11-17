/**
 * How a person reads the alerts inside one group, and which addresses they
 * can follow out of it.
 *
 * The stored order is by start time, and it stays that way: messages and the
 * digest compare it. The modal only lifts firing above stale above resolved.
 * Inside one state the array is left alone. Reading the timestamps again in
 * JS would drop anything finer than a millisecond and could disagree with
 * the row.
 *
 * A dashboard or a runbook is the first non-empty string in a fixed list of
 * keys. That string is then checked. An unusable one is not replaced by a
 * later key, so a javascript: annotation does not reveal the label behind it.
 *
 * The check looks at the raw string before new URL(). The browser rewrites
 * what it parses: it drops tabs and newlines, it reads "\" as "/", and it
 * accepts http forms this product does not. The same addresses are refused
 * here and in Go.
 */

const DASHBOARD_KEYS = [
    ['annotations', 'dashboard'],
    ['annotations', 'dashboard_url'],
    ['labels', 'dashboard'],
    ['labels', 'dashboard_url'],
];

const RUNBOOK_KEYS = [
    ['annotations', 'runbook'],
    ['annotations', 'runbook_url'],
];

/** firing, then stale, then resolved. The input array is not modified. */
export function readingOrder(alerts) {
    return alerts.slice().sort((a, b) => stateRank(a) - stateRank(b));
}

/**
 * The dashboard and runbook to open for one alert, or '' when the chosen
 * string is missing or cannot be followed.
 */
export function links(alert) {
    return {
        dashboard: openable(firstNonEmpty(alert, DASHBOARD_KEYS)),
        runbook: openable(firstNonEmpty(alert, RUNBOOK_KEYS)),
    };
}

/** Whether raw may be the href of a dashboard or a runbook. */
export function linkable(raw) {
    if (typeof raw !== 'string' || raw.length === 0 || rawRejected(raw) || !httpForm(raw)) {
        return false;
    }
    let parsed;
    try {
        parsed = new URL(raw);
    } catch {
        return false;
    }
    return (parsed.protocol === 'http:' || parsed.protocol === 'https:') && parsed.host !== '';
}

function stateRank(alert) {
    const status = typeof alert?.status === 'string' ? alert.status.toLowerCase() : '';
    if (status !== 'firing') return 2;
    return alert?.staleSince ? 1 : 0;
}

function firstNonEmpty(alert, keys) {
    for (const [bag, key] of keys) {
        const value = alert?.[bag]?.[key];
        if (typeof value === 'string' && value.length > 0) return value;
    }
    return '';
}

function openable(raw) {
    return linkable(raw) ? raw : '';
}

function rawRejected(raw) {
    if (/[<>|\\]/.test(raw)) return true;
    if (/[\u0000-\u001F\u007F-\u009F]/.test(raw)) return true;
    return /\p{White_Space}/u.test(raw);
}

function httpForm(raw) {
    let rest = '';
    if (raw.startsWith('https://')) rest = raw.slice('https://'.length);
    else if (raw.startsWith('http://')) rest = raw.slice('http://'.length);
    else return false;
    if (rest.length === 0) return false;
    const end = rest.search(/[/?#]/);
    const authority = end === -1 ? rest : rest.slice(0, end);
    return authority.length > 0;
}

window.AlertReview = { readingOrder, links, linkable };
