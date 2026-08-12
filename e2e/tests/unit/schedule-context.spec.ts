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
});
