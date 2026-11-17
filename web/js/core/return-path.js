/**
 * Where to send a person after they sign in.
 *
 * A link from outside the product, the alert-group link in a Slack or
 * Telegram card, opens a hash route. Signing in used to drop that route and
 * land on the alert-group list. The route is kept across the login page and
 * across the OIDC round trip, and it is restored afterwards.
 *
 * Only a hash route this UI serves is accepted. The value is a navigation
 * target, so anything that could name another host is refused.
 */

const RETURN_KEY = 'tokay.return';
const MAX_RETURN_LENGTH = 512;
const RETURN_HASH = /^#\/(ops|cfg)(\/[A-Za-z0-9._~%-]*)*$/;

/** A hash route this UI can open again, or ''. */
export function safeReturnHash(raw) {
    if (typeof raw !== 'string' || raw.length === 0 || raw.length > MAX_RETURN_LENGTH) return '';
    return RETURN_HASH.test(raw) ? raw : '';
}

/** Login URL for the route currently open, remembering it for the form. */
export function loginLocation() {
    const hash = safeReturnHash(window.location.hash);
    if (!hash) {
        sessionStorage.removeItem(RETURN_KEY);
        return '/login.html';
    }
    sessionStorage.setItem(RETURN_KEY, hash);
    return '/login.html?next=' + encodeURIComponent(hash);
}

/** Forget a route. Logout uses this so the next sign-in starts clean. */
export function clearReturnPath() {
    sessionStorage.removeItem(RETURN_KEY);
}

/**
 * The route the login page should return to. A next parameter wins. A next
 * parameter that is present and rejected is not replaced by an older stored
 * route.
 */
export function currentReturnPath() {
    const params = new URLSearchParams(window.location.search);
    if (params.has('next')) {
        const fromQuery = safeReturnHash(params.get('next'));
        if (fromQuery) {
            sessionStorage.setItem(RETURN_KEY, fromQuery);
            return fromQuery;
        }
        sessionStorage.removeItem(RETURN_KEY);
        return '';
    }
    const stored = safeReturnHash(sessionStorage.getItem(RETURN_KEY));
    if (!stored) sessionStorage.removeItem(RETURN_KEY);
    return stored;
}

/** Read the route and forget it, so a later sign-in does not reuse it. */
export function consumeReturnPath() {
    const hash = currentReturnPath();
    sessionStorage.removeItem(RETURN_KEY);
    return hash;
}

/** App URL for a route. A hash on its own would stay on /login.html. */
export function homeWithHash(hash) {
    return '/' + safeReturnHash(hash);
}

/** OIDC start URL carrying the same route the password form returns to. */
export function oidcRedirectLocation(hash) {
    const safe = safeReturnHash(hash);
    if (!safe) return '/api/auth/oidc/redirect';
    return '/api/auth/oidc/redirect?next=' + encodeURIComponent(safe);
}

/**
 * Put a stored route back when the address has none. A proxy can drop the
 * fragment from the OIDC callback's Location, and the copy kept in this tab
 * is what still names the page. A route already in the address wins.
 */
export function restoreReturnPath() {
    const hash = safeReturnHash(sessionStorage.getItem(RETURN_KEY));
    sessionStorage.removeItem(RETURN_KEY);
    if (!hash) return;
    const current = window.location.hash;
    if (current && current !== '#' && current !== '#/') return;
    window.location.hash = hash;
}

if (typeof window !== 'undefined') {
    window.ReturnPath = {
        safeReturnHash,
        loginLocation,
        clearReturnPath,
        currentReturnPath,
        consumeReturnPath,
        homeWithHash,
        oidcRedirectLocation,
        restoreReturnPath,
    };
}
