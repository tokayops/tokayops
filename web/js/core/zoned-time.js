/**
 * Local wall-clock time to an absolute instant, in a named time zone.
 *
 * A date and a time in a zone do not always name exactly one moment. On the
 * spring transition some local times never happen; on the autumn one some
 * happen twice. The naive conversion - take the zone's current offset and
 * subtract it - is wrong in both cases and wrong silently: 02:30 on the
 * morning Europe/Berlin skips becomes 01:30 local, an hour before what was
 * typed, and 02:30 on the morning it repeats becomes whichever of the two
 * moments the offset happened to be.
 *
 * That matters here because overrides are append-only. A misconverted instant
 * is not a display bug that a re-render fixes; it is the recorded fact about
 * when someone was on duty.
 *
 * So the conversion enumerates instead of assuming: it collects the offsets
 * the zone actually uses around the date, builds one candidate instant per
 * offset, and keeps only those that render back to exactly the local time that
 * was typed. What is left says which case this is.
 */

/** How far to look for transitions. Comfortably past the largest DST shift. */
const SCAN_HOURS = 36;

const PART_FORMATTERS = new Map();

function partsFormatter(timeZone) {
    let formatter = PART_FORMATTERS.get(timeZone);
    if (!formatter) {
        formatter = new Intl.DateTimeFormat('en-US', {
            timeZone,
            year: 'numeric', month: '2-digit', day: '2-digit',
            hour: '2-digit', minute: '2-digit', second: '2-digit',
            hourCycle: 'h23',
        });
        PART_FORMATTERS.set(timeZone, formatter);
    }
    return formatter;
}

/**
 * The wall-clock fields an instant shows in a zone.
 *
 * hour is normalized: with hour12 false some engines render midnight as 24
 * rather than 00, and comparing that against typed input would report a
 * perfectly ordinary midnight as nonexistent.
 */
function zonedParts(instant, timeZone) {
    const parts = {};
    for (const part of partsFormatter(timeZone).formatToParts(instant)) {
        if (part.type !== 'literal') parts[part.type] = parseInt(part.value, 10);
    }
    if (parts.hour === 24) parts.hour = 0;
    return parts;
}

/** The zone's UTC offset at an instant, in milliseconds. */
function offsetAt(instant, timeZone) {
    const p = zonedParts(instant, timeZone);
    return Date.UTC(p.year, p.month - 1, p.day, p.hour, p.minute, p.second) - instant.getTime();
}

/** Parse "YYYY-MM-DDTHH:MM" into its fields, or null if it is not that. */
function parseNaive(value) {
    const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/.exec(value || '');
    if (!match) return null;
    const [, year, month, day, hour, minute] = match.map(Number);
    return { year, month, day, hour, minute };
}

/**
 * Format a zone's offset at an instant, e.g. "UTC+02:00".
 *
 * Shown next to an ambiguous time so the choice between the two moments is
 * visible rather than implied.
 */
export function formatOffset(instant, timeZone) {
    const minutes = Math.round(offsetAt(instant, timeZone) / 60000);
    const sign = minutes < 0 ? '-' : '+';
    const abs = Math.abs(minutes);
    const pad = (n) => String(n).padStart(2, '0');
    return `UTC${sign}${pad(Math.floor(abs / 60))}:${pad(abs % 60)}`;
}

/**
 * Render an instant as a "YYYY-MM-DDTHH:MM" local value for a datetime input.
 * @param {Date|string} instant
 * @param {string} timeZone
 */
export function instantToLocalInput(instant, timeZone) {
    const date = instant instanceof Date ? instant : new Date(instant);
    const p = zonedParts(date, timeZone);
    const pad = (n) => String(n).padStart(2, '0');
    return `${p.year}-${pad(p.month)}-${pad(p.day)}T${pad(p.hour)}:${pad(p.minute)}`;
}

/**
 * Resolve a local wall-clock time in a zone to an absolute instant.
 *
 * @param {string} naiveLocal - "YYYY-MM-DDTHH:MM", as a datetime-local input gives it
 * @param {string} timeZone - IANA name
 * @param {Object} [options]
 * @param {'earlier'|'later'} [options.prefer='earlier'] - which moment to take
 *        when the local time happens twice
 * @returns {{kind: 'ok'|'gap'|'ambiguous'|'invalid', instant: Date|null,
 *           candidates: Date[], offsetLabel: string}}
 *
 * `gap` means the local time does not exist in that zone. It is reported, not
 * repaired: shifting it by an hour would silently record a different time than
 * the one someone entered.
 *
 * `ambiguous` means it exists twice. Both are returned; `instant` is the one
 * `prefer` asked for.
 */
export function resolveLocalTime(naiveLocal, timeZone, options = {}) {
    const prefer = options.prefer === 'later' ? 'later' : 'earlier';
    const fields = parseNaive(naiveLocal);
    if (!fields || !timeZone) {
        return { kind: 'invalid', instant: null, candidates: [], offsetLabel: '' };
    }

    const asIfUTC = Date.UTC(fields.year, fields.month - 1, fields.day, fields.hour, fields.minute);

    // The offsets the zone actually uses around this date. Sampling a window
    // rather than assuming one offset is what makes a transition visible at
    // all; the window is wider than any shift so both sides are always in it.
    const offsets = new Set();
    for (let hours = -SCAN_HOURS; hours <= SCAN_HOURS; hours++) {
        offsets.add(offsetAt(new Date(asIfUTC + hours * 3600000), timeZone));
    }

    // One candidate per offset, kept only if it renders back to exactly what
    // was typed. That round-trip is the whole test: a skipped local time
    // matches no candidate, a repeated one matches two.
    const seen = new Set();
    const candidates = [];
    for (const offset of offsets) {
        const candidate = new Date(asIfUTC - offset);
        if (seen.has(candidate.getTime())) continue;
        const p = zonedParts(candidate, timeZone);
        if (p.year === fields.year && p.month === fields.month && p.day === fields.day &&
            p.hour === fields.hour && p.minute === fields.minute) {
            seen.add(candidate.getTime());
            candidates.push(candidate);
        }
    }
    candidates.sort((a, b) => a - b);

    if (candidates.length === 0) {
        return { kind: 'gap', instant: null, candidates: [], offsetLabel: '' };
    }
    if (candidates.length === 1) {
        return {
            kind: 'ok',
            instant: candidates[0],
            candidates,
            offsetLabel: formatOffset(candidates[0], timeZone),
        };
    }

    const chosen = prefer === 'later' ? candidates[candidates.length - 1] : candidates[0];
    return {
        kind: 'ambiguous',
        instant: chosen,
        candidates,
        offsetLabel: formatOffset(chosen, timeZone),
    };
}

/**
 * Resolve the two ends of a duty window.
 *
 * The ends prefer opposite moments when a local time is ambiguous: the start
 * takes the earlier one and the end the later, so an ambiguous hour widens the
 * window rather than narrowing it. Coverage that is accidentally an hour
 * shorter than intended is a gap in the on-call rota; coverage an hour longer
 * is an overlap someone can see. Both choices are reported so neither is
 * silent.
 *
 * @param {string} fromLocal - "YYYY-MM-DDTHH:MM"
 * @param {string} toLocal - "YYYY-MM-DDTHH:MM"
 * @param {string} timeZone
 * @returns {{from: Object, to: Object}} each the result of resolveLocalTime
 */
export function resolveWindow(fromLocal, toLocal, timeZone) {
    return {
        from: resolveLocalTime(fromLocal, timeZone, { prefer: 'earlier' }),
        to: resolveLocalTime(toLocal, timeZone, { prefer: 'later' }),
    };
}

/**
 * The message for a local time that does not exist.
 * @param {string} timeZone
 */
export function gapMessage(timeZone) {
    return `This local time does not exist in ${timeZone} (daylight saving change). Pick another time.`;
}
