import { test, expect, Page } from '@playwright/test';

/**
 * What state a team's schedule is in, and who gets to decide.
 *
 * There were four booleans - exists, active, deletedAt, unavailable - and
 * three places that worked out the state from them: the attribute written into
 * the DOM, the label under the name, and the badge in the on-call row. They
 * agreed because their branches were in the same order, not because they could
 * not disagree, and `active && deleted` was a thing anyone could write down.
 *
 * Now the state is decided once and carried, and everything else switches on
 * it. These tests are about that: the four states are read rather than
 * re-derived, a fifth one nobody handled is loud, and exists/active are
 * questions about the state rather than fields that can drift from it.
 */

const SHARED = '/js/modules/schedule-shared.js';
const COMPONENTS = '/js/modules/schedule-components.js';
const PERMISSIONS = '/js/modules/permissions.js';
const CALENDAR = '/js/modules/schedule-calendar.js';

type Rendered = {
  kind: string;
  state: string;
  exists: boolean;
  active: boolean;
  status: string;
  widgetState: string | undefined;
  rowState: string | undefined;
  offersCalendar: boolean;
  offersOverride: boolean;
  rowStatus: string | undefined;
};

async function renderAllKinds(page: Page): Promise<Rendered[]> {
  return page.evaluate(async (modules: string[]) => {
    const [sharedUrl, componentsUrl, permissionsUrl] = modules;
    const shared = await import(sharedUrl);
    const components = await import(componentsUrl);
    const { Permissions } = await import(permissionsUrl);
    Permissions.init({ id: 'u1', name: 'Ann', role: 'admin', teams: {} });

    const attribute = (html: string) => /data-schedule-state="([^"]+)"/.exec(html)?.[1];

    return ['active', 'deleted', 'absent', 'unavailable'].map(kind => {
      const ctx = {
        teamId: 'team-1',
        kind,
        scheduleId: kind === 'absent' || kind === 'unavailable' ? '' : 'sch-1',
        deletedAt: kind === 'deleted' ? '2026-02-01T00:00:00Z' : null,
        names: new Map(),
      };

      const widget = components.onCallWidget(null, ctx);
      const row = components.onCallOverviewRow(null, { id: 'team-1', name: 'Team' }, ctx);

      return {
        kind,
        state: components.scheduleState(ctx),
        exists: shared.scheduleExists(kind),
        active: shared.scheduleActive(kind),
        status: components.onCallStatus(ctx, '').label,
        widgetState: attribute(widget),
        rowState: attribute(row),
        offersCalendar: widget.includes('view-schedule-btn'),
        offersOverride: widget.includes('create-override-btn'),
        rowStatus: /oncall-status-badge ([a-z]+)"/.exec(row)?.[1],
      };
    });
  }, [SHARED, COMPONENTS, PERMISSIONS]);
}

test.describe('widget context', () => {
  test.beforeEach(async ({ page }) => {
    // A blank page on the app's origin: the modules resolve, and nothing else
    // runs. These functions take data and return strings, so there is nothing
    // for the app around them to contribute.
    await page.route('**/__schedule-context-harness', route =>
      route.fulfill({ contentType: 'text/html', body: '<!doctype html><title>harness</title>' }));
    await page.goto('/__schedule-context-harness');
  });

  test('the four states say four different things', async ({ page }) => {
    const rendered = await renderAllKinds(page);
    const byKind = Object.fromEntries(rendered.map(r => [r.kind, r]));

    // A running schedule with nobody up right now: the calendar is worth
    // offering, and so is an override.
    expect(byKind.active).toMatchObject({
      state: 'active',
      exists: true,
      active: true,
      status: 'No one on duty',
      offersCalendar: true,
      offersOverride: true,
    });

    // Deleted keeps its past shifts, so the calendar stays; there is nothing
    // to override any more.
    expect(byKind.deleted).toMatchObject({
      state: 'deleted',
      exists: true,
      active: false,
      status: 'Schedule deleted',
      offersCalendar: true,
      offersOverride: false,
    });

    // Never configured: the calendar would open onto a 404.
    expect(byKind.absent).toMatchObject({
      state: 'absent',
      exists: false,
      active: false,
      status: 'Not configured',
      offersCalendar: false,
      offersOverride: false,
    });

    // And the one that is not a fact about the schedule at all. Rendering this
    // as "not configured" turns an outage into a reassuring blank.
    expect(byKind.unavailable).toMatchObject({
      state: 'unavailable',
      exists: false,
      active: false,
      status: 'On-call unavailable',
      offersCalendar: false,
      offersOverride: false,
    });
  });

  test('the widget, the row and the attribute cannot disagree', async ({ page }) => {
    const rendered = await renderAllKinds(page);

    for (const state of rendered) {
      expect(state.widgetState, `widget for ${state.kind}`).toBe(state.kind);
      expect(state.rowState, `row for ${state.kind}`).toBe(state.kind);
    }

    // The row says its own words for the same states - "Unavailable" is not
    // "Not configured" - and it reaches them from the same one place.
    const byKind = Object.fromEntries(rendered.map(r => [r.kind, r]));
    expect(byKind.unavailable.rowStatus).toBe('unavailable');
    expect(byKind.absent.rowStatus).toBe('unconfigured');
    expect(byKind.deleted.rowStatus).toBe('unconfigured');
  });

  test('a state nobody handled is loud', async ({ page }) => {
    const failures = await page.evaluate(async (modules: string[]) => {
      const [sharedUrl, componentsUrl, permissionsUrl] = modules;
      const shared = await import(sharedUrl);
      const components = await import(componentsUrl);
      const { Permissions } = await import(permissionsUrl);
      Permissions.init({ id: 'u1', name: 'Ann', role: 'admin', teams: {} });

      const ctx = { teamId: 'team-1', kind: 'archived', scheduleId: 'sch-1', names: new Map() };
      const attempt = (fn: () => unknown) => {
        try {
          fn();
          return null;
        } catch (error) {
          return (error as Error).message;
        }
      };

      return {
        state: attempt(() => components.scheduleState(ctx)),
        status: attempt(() => components.onCallStatus(ctx, '')),
        widget: attempt(() => components.onCallWidget(null, ctx)),
        row: attempt(() => components.onCallOverviewRow(null, { id: 'team-1' }, ctx)),
        exists: attempt(() => shared.scheduleExists('archived')),
        active: attempt(() => shared.scheduleActive('archived')),
      };
    }, [SHARED, COMPONENTS, PERMISSIONS]);

    // A fifth state has to be handled everywhere it matters, and the way to
    // make sure of that is for the unhandled case to stop rather than to
    // quietly render the least alarming answer.
    for (const [where, message] of Object.entries(failures)) {
      expect(message, `${where} accepted a state it does not know`).toContain('Unknown schedule kind');
    }
  });

  test('the state is derived in one place, and exists/active are not stored', async ({ page }) => {
    const derived = await page.evaluate(async (sharedUrl: string) => {
      const shared = await import(sharedUrl);
      const context = (state: any) => shared.scheduleContext('team-1', state);

      return {
        absent: context({ scheduleId: '', deletedAt: null }).kind,
        active: context({ scheduleId: 'sch-1', deletedAt: null }).kind,
        deleted: context({ scheduleId: 'sch-1', deletedAt: '2026-02-01T00:00:00Z' }).kind,
        unavailable: shared.unavailableContext('team-1').kind,
        keys: Object.keys(context({ scheduleId: 'sch-1', deletedAt: null })),
      };
    }, SHARED);

    expect(derived.absent).toBe('absent');
    expect(derived.active).toBe('active');
    expect(derived.deleted).toBe('deleted');
    expect(derived.unavailable).toBe('unavailable');

    // Stored answers are answers that can drift from the question. `active &&
    // deleted` was expressible for as long as both were fields.
    expect(derived.keys).not.toContain('exists');
    expect(derived.keys).not.toContain('active');
  });

  test('the keep line says when this assignment ends', async ({ page }) => {
    const rendered = await page.evaluate(async (componentsUrl: string) => {
      const components = await import(componentsUrl);
      const names = new Map([['u1', 'Ann'], ['u2', 'Ben']]);
      const groups = [
        { id: 'g1', user_ids: ['u1'] },
        { id: 'g2', user_ids: ['u2'] },
      ];
      // 18:00 in Moscow, and the weekly handoff the Monday after. The two
      // have to format differently, or the assertion cannot tell which
      // boundary the label used.
      const assignmentEnd = '2026-11-17T15:00:00Z';
      const gridEnd = '2026-11-23T08:00:00Z';
      const fmt = (value: string) => new Date(value).toLocaleString(undefined, {
        month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', timeZone: 'Europe/Moscow',
      });
      const choice = (current: Record<string, unknown>) => components.scheduleHandoffChoice({
        groups, names, timezone: 'Europe/Moscow', handoffTo: '', current,
      });

      return {
        override: choice({
          source: 'override', user_ids: ['u1'], group_id: 'g1',
          assignment_end: assignmentEnd, grid_slot_end: gridEnd,
        }),
        rotation: choice({
          source: 'rotation', user_ids: ['u1'], group_id: 'g1',
          assignment_end: gridEnd, grid_slot_end: gridEnd,
        }),
        // The grid is known and the assignment is not. Guessing the grid
        // would put the weekly handoff on a duty that has no end we can name.
        noAssignment: choice({
          source: 'override', user_ids: ['u1'], group_id: 'g1',
          grid_slot_end: gridEnd,
        }),
        assignmentText: fmt(assignmentEnd),
        gridText: fmt(gridEnd),
      };
    }, COMPONENTS);

    expect(rendered.override).toContain(`stays on duty until ${rendered.assignmentText}`);
    expect(rendered.override).not.toContain(rendered.gridText);

    expect(rendered.rotation).toContain(`stays on duty until ${rendered.gridText}`);

    expect(rendered.noAssignment).toContain('stays on duty');
    expect(rendered.noAssignment).not.toContain('until');
    expect(rendered.noAssignment).not.toContain(rendered.gridText);
  });

  test('a handoff day shows when the shift changes, not when the calendar was opened', async ({ page }) => {
    const rendered = await page.evaluate(async (componentsUrl: string) => {
      const components = await import(componentsUrl);
      const zone = 'Asia/Bangkok';
      const clock = (value: string) => new Date(value).toLocaleTimeString('en-US', {
        hour: '2-digit', minute: '2-digit', hour12: false, timeZone: zone,
      });
      // Opened Friday 14:18 in Bangkok. Denis has been on since before that;
      // the render clips his shift to that instant. He hands off Monday at
      // 15:00 Bangkok, which is 11:00 in Moscow.
      const openedAt = '2026-11-13T07:18:00Z';
      const handoff = '2026-11-16T08:00:00Z';
      const html = components.monthlyScheduleCalendar({
        entries: [
          { layer: 'l1', source: 'rotation', user_ids: ['u1'], start: openedAt, end: handoff },
          { layer: 'l1', source: 'rotation', user_ids: ['u2'], start: handoff, end: '2026-11-23T08:00:00Z' },
          // A shift that begins and ends on Tuesday, so the same-day form stays.
          { layer: 'l1', source: 'override', user_ids: ['u3'], start: '2026-11-17T03:00:00Z', end: '2026-11-17T05:00:00Z' },
          { layer: 'l1', source: 'rotation', user_ids: ['u2'], start: '2026-11-17T05:00:00Z', end: '2026-11-23T08:00:00Z' },
        ],
      }, new Date(openedAt), zone, new Map([['u1', 'Denis'], ['u2', 'Yuriy'], ['u3', 'Nikita']]));

      const doc = new DOMParser().parseFromString(html, 'text/html');
      const day = (num: string, month: string) => {
        const block = [...doc.querySelectorAll('.calendar-day')].find(el => {
          const date = el.querySelector('.calendar-day-date')?.textContent || '';
          return date.includes(num) && date.includes(month);
        });
        return [...(block?.querySelectorAll('.calendar-entry') || [])].map(el => ({
          user: el.querySelector('.entry-user')?.textContent || '',
          time: el.querySelector('.entry-time')?.textContent || '',
        }));
      };
      return {
        monday: day('16', 'Nov'),
        tuesday: day('17', 'Nov'),
        opened: clock(openedAt),
        handoff: clock(handoff),
        overrideStart: clock('2026-11-17T03:00:00Z'),
        overrideEnd: clock('2026-11-17T05:00:00Z'),
      };
    }, COMPONENTS);

    const denis = rendered.monday.find(entry => entry.user === 'Denis');
    const yuriy = rendered.monday.find(entry => entry.user === 'Yuriy');
    expect(denis?.time).toBe(`→${rendered.handoff}`);
    expect(yuriy?.time).toBe(`${rendered.handoff}→`);
    expect(rendered.monday.map(entry => entry.time).join(' ')).not.toContain(rendered.opened);

    const nikita = rendered.tuesday.find(entry => entry.user === 'Nikita');
    expect(nikita?.time).toBe(`${rendered.overrideStart}-${rendered.overrideEnd}`);
  });

  test('opening on the handoff day does not show the open time as coming on duty', async ({ page }) => {
    const rendered = await page.evaluate(async (componentsUrl: string) => {
      const components = await import(componentsUrl);
      const zone = 'Europe/Moscow';
      const clock = (value: string) => new Date(value).toLocaleTimeString('en-US', {
        hour: '2-digit', minute: '2-digit', hour12: false, timeZone: zone,
      });
      // Monday 10:18 in Moscow, fifty minutes before the 11:00 handoff.
      // The render clips Denis to that instant. It is the window edge, not
      // when his duty began.
      const openedAt = '2026-11-16T07:18:00Z';
      const handoff = '2026-11-16T08:00:00Z';
      const html = components.monthlyScheduleCalendar({
        entries: [
          { layer: 'l1', source: 'rotation', user_ids: ['u1'], start: openedAt, end: handoff },
          { layer: 'l1', source: 'rotation', user_ids: ['u2'], start: handoff, end: '2026-11-23T08:00:00Z' },
        ],
      }, new Date(openedAt), zone, new Map([['u1', 'Denis'], ['u2', 'Yuriy']]));
      const doc = new DOMParser().parseFromString(html, 'text/html');
      const monday = [...doc.querySelectorAll('.calendar-day')].find(el =>
        (el.querySelector('.calendar-day-date')?.textContent || '').trim() === '16 Nov');
      const entries = [...(monday?.querySelectorAll('.calendar-entry') || [])].map(el => ({
        user: el.querySelector('.entry-user')?.textContent || '',
        time: el.querySelector('.entry-time')?.textContent || '',
      }));
      return { entries, opened: clock(openedAt), handoff: clock(handoff) };
    }, COMPONENTS);

    const denis = rendered.entries.find(entry => entry.user === 'Denis');
    const yuriy = rendered.entries.find(entry => entry.user === 'Yuriy');
    expect(denis?.time).toBe(`→${rendered.handoff}`);
    expect(denis?.time).not.toContain(rendered.opened);
    expect(yuriy?.time).toBe(`${rendered.handoff}→`);
  });

  test('a day whose midnight was skipped still labels the handoff, not the whole shift', async ({ page }) => {
    const rendered = await page.evaluate(async (componentsUrl: string) => {
      const components = await import(componentsUrl);
      const zone = 'America/Santiago';
      const clock = (value: string) => new Date(value).toLocaleTimeString('en-US', {
        hour: '2-digit', minute: '2-digit', hour12: false, timeZone: zone,
      });
      // 6 September 2026 has no midnight there: the clocks jump from
      // 23:59 on the 5th to 01:00 on the 6th. Denis was clipped to an
      // earlier open; he hands off at 11:00 that morning.
      const openedAt = '2026-09-04T14:18:00Z';
      const handoff = '2026-09-06T14:00:00Z';
      const html = components.monthlyScheduleCalendar({
        entries: [
          { layer: 'l1', source: 'rotation', user_ids: ['u1'], start: openedAt, end: handoff },
          { layer: 'l1', source: 'rotation', user_ids: ['u2'], start: handoff, end: '2026-09-13T14:00:00Z' },
        ],
      }, new Date(openedAt), zone, new Map([['u1', 'Denis'], ['u2', 'Yuriy']]));
      const doc = new DOMParser().parseFromString(html, 'text/html');
      const day = [...doc.querySelectorAll('.calendar-day')].find(el =>
        (el.querySelector('.calendar-day-date')?.textContent || '').trim() === '6 Sep');
      const entries = [...(day?.querySelectorAll('.calendar-entry') || [])].map(el => ({
        user: el.querySelector('.entry-user')?.textContent || '',
        time: el.querySelector('.entry-time')?.textContent || '',
      }));
      return { entries, opened: clock(openedAt), handoff: clock(handoff) };
    }, COMPONENTS);

    const denis = rendered.entries.find(entry => entry.user === 'Denis');
    const yuriy = rendered.entries.find(entry => entry.user === 'Yuriy');
    expect(denis?.time).toBe(`→${rendered.handoff}`);
    expect(denis?.time).not.toContain(rendered.opened);
    expect(yuriy?.time).toBe(`${rendered.handoff}→`);
    expect(yuriy?.time).not.toBe(`${rendered.handoff}-${rendered.handoff}`);
  });

  test('the calendar opens in the schedule zone, not the computer zone', async ({ page }) => {
    const chosen = await page.evaluate(async (calendarUrl: string) => {
      const calendar = await import(calendarUrl);
      return {
        schedule: calendar.calendarTimezone({
          schedule: 'Europe/Moscow', browser: 'Asia/Bangkok',
        }),
        remembered: calendar.calendarTimezone({
          stored: 'Asia/Bangkok', schedule: 'Europe/Moscow', browser: 'Asia/Bangkok',
        }),
        computer: calendar.calendarTimezone({ browser: 'Asia/Bangkok' }),
        unknown: calendar.calendarTimezone({
          stored: 'Mars/Olympus', schedule: 'Europe/Moscow', browser: 'Asia/Bangkok',
        }),
      };
    }, CALENDAR);

    expect(chosen.schedule).toBe('Europe/Moscow');
    expect(chosen.remembered).toBe('Asia/Bangkok');
    expect(chosen.computer).toBe('Asia/Bangkok');
    expect(chosen.unknown).toBe('Europe/Moscow');
  });
});
