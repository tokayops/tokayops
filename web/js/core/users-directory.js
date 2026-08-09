/**
 * User display names for historical data.
 *
 * The schedule API answers in user IDs. Turning them into names is the
 * client's job, and it is not the same job as offering people to assign:
 *
 *  - `resolveNames` answers for anyone the system has ever known, including
 *    people who left the team and people who were erased. Their row survives
 *    precisely so history that names their ID stays legible.
 *  - `assignableMembers` answers who may be put on a rotation now: members of
 *    the team who are still active. Offering a departed member would only
 *    produce a rejected save.
 *
 * Names are cached briefly rather than for the session. A name held forever is
 * a name that survives an erasure: another admin erases someone, the server
 * starts answering with what anonymization left behind, and an open tab keeps
 * showing the old name until it is reloaded. A short time-to-live bounds that
 * without making every render a request.
 */

const NAME_TTL_MS = 60 * 1000;

// The server bounds a resolve request; the client splits to stay under it.
const RESOLVE_CHUNK = 500;

/** id -> {name, fetchedAt}; name is null for an id the server does not know. */
const names = new Map();

/**
 * Two separate things, easy to confuse.
 *
 * `pendingIds` is what has been asked for during this tick and not sent yet;
 * coalescing it means several renderers running back to back make one request
 * instead of one each.
 *
 * `inFlight` is what is already being fetched. It is not the same as having a
 * batch queued: the batch slot frees as soon as its request is dispatched, so
 * without this a caller arriving while the network is busy would ask for the
 * very same ids again. Keeping both is what makes "one request per id in
 * flight" true rather than merely intended.
 */
let pendingIds = new Set();
let pendingFlush = null;
const inFlight = new Map();

function isFresh(entry, now) {
    return entry && (now - entry.fetchedAt) < NAME_TTL_MS;
}

function fetchChunk(chunk) {
    const request = API.users.resolve(chunk)
        .then(response => {
            const fetchedAt = Date.now();
            const resolved = new Set();
            for (const user of response?.users || []) {
                names.set(user.id, { name: user.name, fetchedAt });
                resolved.add(user.id);
            }
            // An id the server does not know is an answer too, and it is
            // cached like any other. Without this, every render of a calendar
            // naming a since-purged user asks again, for the life of the page.
            for (const id of chunk) {
                if (!resolved.has(id)) names.set(id, { name: null, fetchedAt });
            }
        })
        .catch(error => {
            // Deliberately left uncached: a failed lookup is not evidence that
            // the person does not exist, so the next render retries.
            console.warn('Failed to resolve user names', error);
        })
        .finally(() => {
            for (const id of chunk) {
                if (inFlight.get(id) === request) inFlight.delete(id);
            }
        });

    for (const id of chunk) inFlight.set(id, request);
    return request;
}

function scheduleFlush() {
    if (pendingFlush) return pendingFlush;

    pendingFlush = Promise.resolve().then(() => {
        const ids = [...pendingIds];
        pendingIds = new Set();
        // Freed here, before the network work: the next tick gets its own
        // batch. Requests already dispatched are tracked by inFlight.
        pendingFlush = null;
        if (ids.length === 0) return;

        const requests = [];
        for (let i = 0; i < ids.length; i += RESOLVE_CHUNK) {
            requests.push(fetchChunk(ids.slice(i, i + RESOLVE_CHUNK)));
        }
        return Promise.all(requests);
    });

    return pendingFlush;
}

/**
 * Resolve display names for a set of user IDs.
 *
 * IDs the server does not know stay out of the returned map; the caller
 * decides how to render an unknown person, and a placeholder here would have
 * to be told apart from a real name later.
 *
 * @param {string[]} ids
 * @returns {Promise<Map<string, string>>} id -> name, for the ids that resolved
 */
export async function resolveNames(ids) {
    const wanted = [...new Set((ids || []).filter(Boolean))];
    const now = Date.now();

    // Neither cached nor already on its way.
    const missing = wanted.filter(id => !isFresh(names.get(id), now) && !inFlight.has(id));
    if (missing.length > 0) {
        for (const id of missing) pendingIds.add(id);
        await scheduleFlush();
    }

    // Then wait on anything covering a wanted id that someone else started,
    // so two panels rendering the same shift resolve it once between them.
    await Promise.all(wanted.map(id => inFlight.get(id)).filter(Boolean));

    const out = new Map();
    for (const id of wanted) {
        const entry = names.get(id);
        if (entry?.name) out.set(id, entry.name);
    }
    return out;
}

/**
 * The name to show for one ID, given an already-resolved map.
 *
 * An unresolved ID is labelled rather than hidden: a shift with a name missing
 * still happened, and dropping the person would make the calendar quietly
 * wrong instead of visibly incomplete.
 *
 * @param {Map<string, string>} resolved
 * @param {string} id
 */
export function displayName(resolved, id) {
    if (!id) return 'Unknown user';
    return resolved.get(id) || `Unknown user (${id})`;
}

/**
 * Resolve and join a list of IDs, in the order given.
 * @param {string[]} ids
 * @returns {Promise<string>}
 */
export async function joinNames(ids) {
    const resolved = await resolveNames(ids);
    return (ids || []).map(id => displayName(resolved, id)).join(', ');
}

/**
 * Who may be assigned to a rotation or an override right now.
 *
 * Team membership is the whole answer. Erasing someone removes their
 * memberships in the same transaction that marks them erased, and that is the
 * only way a user is ever soft-deleted - so a team's member list cannot name
 * an erased person.
 *
 * This used to intersect the member list with the full user list, guarding
 * against a state that cannot occur, at the cost of fetching every user in the
 * installation each time an editor was opened.
 *
 * Someone who left the team is a different matter: they are still named by
 * past shifts, which is what resolveNames is for, and they are correctly
 * absent here.
 *
 * @param {string} teamId
 * @returns {Promise<Array<{id, name}>>} in the order the team lists them
 */
export async function assignableMembers(teamId) {
    const response = await API.teams.members(teamId);
    return (response?.users || []).map(member => ({ id: member.id, name: member.name }));
}

/**
 * Forget cached names.
 *
 * Called after this tab changes or erases a user: the TTL would get there
 * eventually, but showing a name the tab itself just removed is the one case
 * where eventually is plainly wrong.
 *
 * @param {string} [id] - one user, or all of them when omitted
 */
export function invalidateNames(id) {
    if (id) {
        names.delete(id);
        return;
    }
    names.clear();
}
