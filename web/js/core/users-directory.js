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

/** id -> {name, fetchedAt} */
const names = new Map();

/**
 * IDs asked for during this tick, and the flush that will fetch them.
 *
 * Batching is per tick rather than per call because the callers are renderers:
 * the on-call page draws a row per team, and each row asks about its own two
 * or three people. Resolving per call made that one request per row - a number
 * that grows with the number of teams, on a page that is opened constantly.
 * Coalescing costs one microtask and turns the page back into one request.
 */
let pendingIds = new Set();
let pendingFlush = null;

function isFresh(entry, now) {
    return entry && (now - entry.fetchedAt) < NAME_TTL_MS;
}

function scheduleFlush() {
    if (pendingFlush) return pendingFlush;

    pendingFlush = Promise.resolve().then(async () => {
        const ids = [...pendingIds];
        pendingIds = new Set();
        pendingFlush = null;
        if (ids.length === 0) return;

        for (let i = 0; i < ids.length; i += RESOLVE_CHUNK) {
            const chunk = ids.slice(i, i + RESOLVE_CHUNK);
            try {
                const response = await API.users.resolve(chunk);
                const fetchedAt = Date.now();
                const resolved = new Set();
                for (const user of response?.users || []) {
                    names.set(user.id, { name: user.name, fetchedAt });
                    resolved.add(user.id);
                }
                // An ID the server does not know is an answer too, and it is
                // cached like any other. Without this, every render of a
                // calendar naming a since-purged user asks again, for the
                // whole life of the page.
                for (const id of chunk) {
                    if (!resolved.has(id)) names.set(id, { name: null, fetchedAt });
                }
            } catch (error) {
                // Deliberately left uncached: a failed lookup is not evidence
                // that the person does not exist, so the next render retries.
                console.warn('Failed to resolve user names', error);
            }
        }
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

    const missing = wanted.filter(id => !isFresh(names.get(id), now));
    if (missing.length > 0) {
        for (const id of missing) pendingIds.add(id);
        // Two renderers asking in the same tick wait on the same flush, so
        // they make one request between them rather than one each.
        await scheduleFlush();
    }

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
 * The intersection of two lists that disagree on purpose: team membership does
 * not exclude erased users, and the user list does. Someone who left the
 * company is still named by past shifts - which is what resolveNames is for -
 * but must not be offered for a new one, because the save would refuse them
 * and the editor would be blaming the user for a choice it presented.
 *
 * @param {string} teamId
 * @returns {Promise<Array<{id, name}>>} in the order the team lists them
 */
export async function assignableMembers(teamId) {
    const [membersResponse, usersResponse] = await Promise.all([
        API.teams.members(teamId),
        API.users.list(),
    ]);

    const active = new Set((usersResponse?.users || []).map(u => u.id));
    return (membersResponse?.users || [])
        .filter(member => active.has(member.id))
        .map(member => ({ id: member.id, name: member.name }));
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
