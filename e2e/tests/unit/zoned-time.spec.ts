import { test, expect, Page } from '@playwright/test';

/**
 * The local-time-to-instant conversion, exercised in the browsers that
 * actually run it.
 *
 * Importing the module into this test runner would prove something about
 * Node's ICU data instead. Time zone rules come from the engine, and the
 * engine here is Chromium or Firefox depending on the project - so the module
 * is loaded from the served page and evaluated there, once per project.
 *
 * These cases exist because an override is append-only: a conversion that is
 * an hour off on a transition day does not render wrong, it records wrong.
 */

/**
 * The module is served by the app, so it is imported at runtime through a
 * variable - a literal specifier would only send the type-checker looking for
 * a file that is not part of this project. It travels as an argument because
 * the body of page.evaluate runs in the browser, where nothing from this
 * file's scope exists.
 */
const MODULE = '/js/core/zoned-time.js';

type Resolved = {
  kind: 'ok' | 'gap' | 'ambiguous' | 'invalid';
  instant: string | null;
  candidates: string[];
  offsetLabel: string;
};

async function resolve(
  page: Page,
  local: string,
  timeZone: string,
  prefer?: 'earlier' | 'later',
): Promise<Resolved> {
  return page.evaluate(async (args: { module: string; local: string; timeZone: string; prefer?: string }) => {
    const mod = await import(args.module);
    const result = mod.resolveLocalTime(args.local, args.timeZone,
      args.prefer ? { prefer: args.prefer } : {});
    return {
      kind: result.kind,
      instant: result.instant ? result.instant.toISOString() : null,
      candidates: result.candidates.map((d: Date) => d.toISOString()),
      offsetLabel: result.offsetLabel,
    };
  }, { module: MODULE, local, timeZone, prefer });
}

test.describe('zoned-time', () => {
  // A blank page on the app's origin, so the module resolves and nothing else
  // runs. Landing on a real page instead would put this suite at the mercy of
  // the app's routing: the login page redirects when a session already exists,
  // and the navigation tears down the context mid-evaluate. The module under
  // test has no use for the app anyway.
  test.beforeEach(async ({ page }) => {
    await page.route('**/__zoned-time-harness', route =>
      route.fulfill({ contentType: 'text/html', body: '<!doctype html><title>harness</title>' }));
    await page.goto('/__zoned-time-harness');
  });

  test('rejects a local time that does not exist', async ({ page }) => {
    // Europe/Berlin skips 02:00-03:00 on 2026-03-29.
    const gap = await resolve(page, '2026-03-29T02:30', 'Europe/Berlin');
    expect(gap.kind).toBe('gap');
    expect(gap.instant).toBeNull();

    // The naive conversion used to answer 00:30Z here, which is 01:30 in
    // Berlin - an hour before what was typed, and silently so.
    const justAfter = await resolve(page, '2026-03-29T03:30', 'Europe/Berlin');
    expect(justAfter.kind).toBe('ok');
    expect(justAfter.instant).toBe('2026-03-29T01:30:00.000Z');
  });

  test('reports both moments when a local time happens twice', async ({ page }) => {
    // Europe/Berlin repeats 02:00-03:00 on 2026-10-25.
    const earlier = await resolve(page, '2026-10-25T02:30', 'Europe/Berlin', 'earlier');
    expect(earlier.kind).toBe('ambiguous');
    expect(earlier.candidates).toEqual([
      '2026-10-25T00:30:00.000Z',
      '2026-10-25T01:30:00.000Z',
    ]);
    expect(earlier.instant).toBe('2026-10-25T00:30:00.000Z');
    expect(earlier.offsetLabel).toBe('UTC+02:00');

    const later = await resolve(page, '2026-10-25T02:30', 'Europe/Berlin', 'later');
    expect(later.instant).toBe('2026-10-25T01:30:00.000Z');
    expect(later.offsetLabel).toBe('UTC+01:00');
  });

  test('handles a half-hour transition', async ({ page }) => {
    // Australia/Lord_Howe shifts by 30 minutes, not an hour: 02:00 becomes
    // 02:30 in October and 02:00 becomes 01:30 in April. An implementation
    // that assumed whole-hour offsets would pass Berlin and fail here.
    const gap = await resolve(page, '2026-10-04T02:15', 'Australia/Lord_Howe');
    expect(gap.kind).toBe('gap');

    const fold = await resolve(page, '2026-04-05T01:45', 'Australia/Lord_Howe', 'earlier');
    expect(fold.kind).toBe('ambiguous');
    expect(fold.candidates).toHaveLength(2);
  });

  test('handles offsets that are not whole hours', async ({ page }) => {
    const kathmandu = await resolve(page, '2026-06-01T12:00', 'Asia/Kathmandu');
    expect(kathmandu.kind).toBe('ok');
    expect(kathmandu.offsetLabel).toBe('UTC+05:45');
    expect(kathmandu.instant).toBe('2026-06-01T06:15:00.000Z');

    const chatham = await resolve(page, '2026-06-01T12:00', 'Pacific/Chatham');
    expect(chatham.kind).toBe('ok');
    expect(chatham.offsetLabel).toBe('UTC+12:45');
  });

  test('handles a zone that skipped a whole calendar day', async ({ page }) => {
    // Pacific/Apia crossed the date line at the end of 2011: 2011-12-30 never
    // happened there. Nothing on that date can resolve.
    const skipped = await resolve(page, '2011-12-30T10:00', 'Pacific/Apia');
    expect(skipped.kind).toBe('gap');

    const nextDay = await resolve(page, '2011-12-31T10:00', 'Pacific/Apia');
    expect(nextDay.kind).toBe('ok');
  });

  test('midnight is ordinary', async ({ page }) => {
    // Some engines render midnight as hour 24; comparing that against typed
    // input would report an ordinary midnight as nonexistent.
    const midnight = await resolve(page, '2026-01-01T00:00', 'Europe/Berlin');
    expect(midnight.kind).toBe('ok');
    expect(midnight.instant).toBe('2025-12-31T23:00:00.000Z');
  });

  test('a duty window widens rather than narrows when ambiguous', async ({ page }) => {
    const result = await page.evaluate(async (module: string) => {
      const mod = await import(module);
      const w = mod.resolveWindow('2026-10-25T02:30', '2026-10-25T02:30', 'Europe/Berlin');
      return {
        fromKind: w.from.kind,
        toKind: w.to.kind,
        from: w.from.instant ? w.from.instant.toISOString() : null,
        to: w.to.instant ? w.to.instant.toISOString() : null,
      };
    }, MODULE);

    expect(result.fromKind).toBe('ambiguous');
    expect(result.toKind).toBe('ambiguous');
    // The start takes the earlier moment and the end the later one, so an
    // ambiguous hour cannot silently shorten the cover.
    expect(result.from).toBe('2026-10-25T00:30:00.000Z');
    expect(result.to).toBe('2026-10-25T01:30:00.000Z');
    expect(new Date(result.to!).getTime()).toBeGreaterThan(new Date(result.from!).getTime());
  });

  /**
   * The fold is what an edit form cannot infer from its own inputs.
   *
   * Both instants below render to the identical local string "02:30", so a
   * form filled from either one and saved back unchanged has to be told which
   * pass it came from. Assuming the earlier one - which is the right default
   * for a value nobody has chosen yet - moves an override anchored to the
   * second pass an hour into the past, silently, on a save that changed
   * nothing.
   */
  test('tells the two passes of a repeated local hour apart', async ({ page }) => {
    const result = await page.evaluate(async (module: string) => {
      const mod = await import(module);
      const tz = 'Europe/Berlin';
      const first = '2026-10-25T00:30:00Z';
      const second = '2026-10-25T01:30:00Z';
      return {
        firstLocal: mod.instantToLocalInput(first, tz),
        secondLocal: mod.instantToLocalInput(second, tz),
        firstFold: mod.foldOf(first, tz),
        secondFold: mod.foldOf(second, tz),
        plainFold: mod.foldOf('2026-07-01T10:00:00Z', tz),
        // The API takes RFC3339, so a stored override can carry seconds. The
        // candidates this is compared against are built from a minute-precision
        // local string, so an exact timestamp comparison called every such
        // instant 'later' and moved it an hour on the next save.
        firstWithSeconds: mod.foldOf('2026-10-25T00:30:30Z', tz),
        secondWithSeconds: mod.foldOf('2026-10-25T01:30:30Z', tz),
        // And below the second. PostgreSQL stores microseconds, so a stored
        // override round-trips through the API with a fractional part; the
        // offset comparison has to be immune to it too.
        firstWithMillis: mod.foldOf('2026-10-25T00:30:30.123Z', tz),
        secondWithMillis: mod.foldOf('2026-10-25T01:30:30.123Z', tz),
        lastMomentOfTheFirstPass: mod.foldOf('2026-10-25T00:59:59.999Z', tz),
      };
    }, MODULE);

    // The premise: the two instants are indistinguishable in the input.
    expect(result.firstLocal).toBe('2026-10-25T02:30');
    expect(result.secondLocal).toBe('2026-10-25T02:30');

    expect(result.firstFold).toBe('earlier');
    expect(result.secondFold).toBe('later');
    // An unambiguous instant answers with the default, so filling a form from
    // one is unaffected.
    expect(result.plainFold).toBe('earlier');

    expect(result.firstWithSeconds).toBe('earlier');
    expect(result.secondWithSeconds).toBe('later');
    expect(result.firstWithMillis).toBe('earlier');
    expect(result.secondWithMillis).toBe('later');
    expect(result.lastMomentOfTheFirstPass).toBe('earlier');
  });

  test('round-trips an instant back to a local input value', async ({ page }) => {
    const value = await page.evaluate(async (module: string) => {
      const mod = await import(module);
      return mod.instantToLocalInput(new Date('2026-07-01T10:00:00Z'), 'Europe/Berlin');
    }, MODULE);
    expect(value).toBe('2026-07-01T12:00');
  });
});
