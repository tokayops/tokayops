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

/** id -> Promise, so N renders asking at once make one request. */
const inFlight = new Map();

function isFresh(entry, now) {
    return entry && (now - entry.fetchedAt) < NAME_TTL_MS;
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

    const missing = wanted.filter(id => !isFresh(names.get(id), now) && !inFlight.has(id));
    for (let i = 0; i < missing.length; i += RESOLVE_CHUNK) {
        const chunk = missing.slice(i, i + RESOLVE_CHUNK);
        const request = API.users.resolve(chunk)
            .then(response => {
                const fetchedAt = Date.now();
                for (const user of response?.users || []) {
                    names.set(user.id, { name: user.name, fetchedAt });
                }
                return response;
            })
            .catch(error => {
                console.warn('Failed to resolve user names', error);
                return null;
            })
            .finally(() => {
                for (const id of chunk) inFlight.delete(id);
            });
        for (const id of chunk) inFlight.set(id, request);
    }

    // Wait on every request covering a wanted id, including ones another
    // caller started: two panels rendering the same shift must not race into
    // two identical requests.
    await Promise.all(wanted.map(id => inFlight.get(id)).filter(Boolean));

    const out = new Map();
    for (const id of wanted) {
        const entry = names.get(id);
        if (entry) out.set(id, entry.name);
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
