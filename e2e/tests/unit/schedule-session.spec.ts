import { test, expect, Page } from '@playwright/test';

/**
 * One open of a schedule modal, and what it owns.
 *
 * The feature has three lifetimes, and the bugs came from treating them as
 * one: the dispatcher that opens modals lives as long as the page, the
 * calendar's menu lives as long as the calendar, and everything the override
 * form remembers lives as long as that form is on screen. Taking down "all the
 * listeners" when a modal closed would kill the first two; leaving them up
 * leaked the third into the next modal.
 *
 * These run against the real modules in a real browser, with the API and the
 * page around them stubbed - the questions here are about who owns what, and
 * the answers must not depend on a server being up.
 */

const HARNESS = '/__schedule-session-harness';

const HARNESS_HTML = `<!doctype html><title>harness</title>
<div id="toast-container"></div>
<div id="modal-overlay" class="modal-overlay">
  <div class="modal">
    <div class="modal-header">
      <h3 id="modal-title"></h3>
      <button id="modal-close" class="modal-close">x</button>
    </div>
    <div class="modal-body" id="modal-body"></div>
    <div class="modal-footer" id="modal-footer"></div>
  </div>
</div>
<div id="page"></div>`;

/**
 * Load the feature into a bare page with the modal shell it expects.
 *
 * The listener patch goes in before the modules do, and records where each
 * registration came from. That is what lets a test ask the question that
 * matters - "are this owner's listeners gone, and only this owner's?" - rather
 * than counting listeners and hoping.
 */
async function boot(page: Page) {
  await page.route(`**${HARNESS}`, route =>
    route.fulfill({ contentType: 'text/html', body: HARNESS_HTML }));
  await page.goto(HARNESS);

  await page.evaluate(async () => {
    const w = window as any;

    const harness: any = {
      listeners: [] as any[],
      calls: { created: 0, updated: 0, deleted: 0, overrideSignals: [] as any[] },
      holdOverrides: false,
      releaseOverrides: null as any,
      overrides: [{
        override_id: 'ovr-1',
        revision: 3,
        user_id: 'u1',
        valid_from: new Date(Date.now() + 3600_000).toISOString(),
        valid_to: new Date(Date.now() + 7200_000).toISOString(),
        reason: 'cover',
      }],
    };
    w.__h = harness;

    // The toast markup comes from the classic-script component object, which
    // this page does not load.
    w.Components = { toast: (message: string) => `<div class="toast">${message}</div>` };
    w.confirm = () => true;

    w.API = {
      teams: {
        members: async () => ({ users: [{ id: 'u1', name: 'Ann' }, { id: 'u2', name: 'Ben' }] }),
      },
      users: {
        resolve: async (ids: string[]) => ({ users: ids.map(id => ({ id, name: `User ${id}` })) }),
      },
      schedules: {
        currentOnCall: async () => ({
          schedule_id: 'sch-1',
          on_call: { l1: { user_ids: ['u1'], source: 'rotation' } },
        }),
        getConfig: async () => ({
          schedule_id: 'sch-1',
          version: 1,
          config: {
            timezone: 'UTC',
            slack_usergroup_id: '',
            l1: { enabled: true, rotation_type: 'daily', handoff_time: '09:00', handoff_day: null, groups: [{ id: 'g1', user_ids: ['u1'] }] },
            l2: { enabled: false, escalation_timeout_minutes: 5, rotation_type: 'daily', handoff_time: '09:00', handoff_day: null, user_ids: [] },
          },
        }),
        listOverrides: async (_teamId: string, options: any = {}) => {
          harness.calls.overrideSignals.push(options.signal || null);
          if (harness.holdOverrides) {
            await new Promise(resolve => { harness.releaseOverrides = resolve; });
          }
          return { overrides: harness.overrides };
        },
        render: async () => ({
          entries: [{
            start: new Date(Date.now() - 3600_000).toISOString(),
            end: new Date(Date.now() + 3600_000).toISOString(),
            layer: 'l1',
            source: 'override',
            override_id: 'ovr-1',
            user_ids: ['u1'],
          }],
          warnings: [],
        }),
        createOverride: async () => { harness.calls.created += 1; return {}; },
        updateOverride: async () => { harness.calls.updated += 1; return {}; },
        deleteOverride: async () => { harness.calls.deleted += 1; return {}; },
      },
    };

    const utils = await import('/js/core/utils.js');
    utils.initElements();
    const permissions = await import('/js/modules/permissions.js');
    permissions.Permissions.init({ id: 'u1', name: 'Ann', role: 'admin', teams: {} });

    const original = EventTarget.prototype.addEventListener;
    EventTarget.prototype.addEventListener = function (type: any, handler: any, options: any) {
      // Who registered it, meaning the frame that called this - not anything
      // further up the stack. A shared control that a modal initialises binds
      // listeners of its own, and those are its business, not the modal's.
      const frames = (new Error().stack || '').split('\n');
      const from = frames.find(frame => frame.includes('/js/')) || '';
      harness.listeners.push({ type, options, from });
      return original.call(this, type, handler, options);
    };

    const schedules = await import('/js/modules/schedules.js');
    schedules.bindScheduleEvents();
    harness.mark = harness.listeners.length;

    document.getElementById('page')!.innerHTML = `
      <div class="on-call-widget" data-team-id="team-1">
        <button class="create-override-btn" data-team-id="team-1">Override</button>
        <button class="view-schedule-btn" data-team-id="team-1">View</button>
        <button class="edit-schedule-btn" data-team-id="team-1">Configure</button>
      </div>`;
  });
}

/** What the schedule modules registered while a modal was open, and its fate. */
async function sessionListeners(page: Page) {
  return page.evaluate(() => {
    const harness = (window as any).__h;
    const owned = (entry: any) => entry.from.includes('/js/modules/schedule-')
      || entry.from.includes('/js/core/modal-session.js');

    return {
      session: harness.listeners.slice(harness.mark).filter(owned)
        .map((entry: any) => ({ type: entry.type, aborted: !!entry.options?.signal?.aborted })),
      // Everything registered before the first modal was opened: the delegated
      // dispatcher, and whatever the page itself set up.
      page: harness.listeners.slice(0, harness.mark)
        .map((entry: any) => ({ type: entry.type, aborted: !!entry.options?.signal?.aborted })),
    };
  });
}

async function openOverrideModal(page: Page) {
  await page.locator('.create-override-btn').click();
  await page.locator('#override-form').waitFor({ state: 'attached' });
}

test.describe('modal session', () => {
  test.beforeEach(async ({ page }) => {
    await boot(page);
  });

  test('close() is idempotent, and aborts once', async ({ page }) => {
    const result = await page.evaluate(async () => {
      const { beginModalSession } = await import('/js/core/modal-session.js');
      const session = beginModalSession();

      let aborts = 0;
      session.signal.addEventListener('abort', () => { aborts += 1; });

      session.close();
      session.close();
      session.closeModal();

      return { aborts, closed: session.closed };
    });

    // Four ways out of a modal run into each other: cancel closes and then
    // Escape arrives, or the X is clicked twice. The second one through has to
    // be a no-op, not a second teardown.
    expect(result.aborts, 'one open, one teardown').toBe(1);
    expect(result.closed).toBe(true);
  });

  test('a new modal starts a session the old one cannot reach', async ({ page }) => {
    const result = await page.evaluate(async () => {
      const { beginModalSession } = await import('/js/core/modal-session.js');
      const first = beginModalSession();
      const second = beginModalSession();
      return { first: first.closed, second: second.closed };
    });

    expect(result.first, 'opening a modal ends the one it replaced').toBe(true);
    expect(result.second).toBe(false);
  });

  for (const exit of ['cancel', 'close button', 'overlay', 'Escape'] as const) {
    test(`leaving by ${exit} takes the override session's listeners, and only those`, async ({ page }) => {
      await openOverrideModal(page);

      const open = await sessionListeners(page);
      expect(open.session.length, 'the modal registered listeners of its own')
        .toBeGreaterThan(0);
      expect(open.session.every(l => !l.aborted)).toBe(true);

      switch (exit) {
        case 'cancel':
          await page.locator('#override-cancel').click();
          break;
        case 'close button':
          await page.locator('#modal-close').click();
          break;
        case 'overlay':
          await page.evaluate(() => document.getElementById('modal-overlay')!.click());
          break;
        case 'Escape':
          await page.keyboard.press('Escape');
          break;
      }

      const closed = await sessionListeners(page);
      expect(closed.session.filter(l => !l.aborted),
        'a listener that outlives its modal is the leak this replaced').toEqual([]);
      expect(closed.page.filter(l => l.aborted),
        'the dispatcher that opens modals must survive them').toEqual([]);

      // And it survives in the way that matters: the button still works.
      await openOverrideModal(page);
      await expect(page.locator('#override-form')).toBeAttached();
    });
  }

  test('the calendar keeps its own listeners while an override modal comes and goes',
    async ({ page }) => {
      await page.locator('.view-schedule-btn').click();
      await page.locator('.calendar-list').waitFor({ state: 'attached' });

      const entry = page.locator('.calendar-entry.layer-override').first();
      await entry.click();
      await expect(page.locator('.override-context-menu.active')).toBeAttached();

      // Editing leaves the calendar for the override modal.
      await page.locator('.override-context-menu-item[data-action="edit"]').click();
      await page.locator('#override-form').waitFor({ state: 'attached' });

      // Escape means "back to the calendar" here, not "close everything".
      await page.keyboard.press('Escape');
      await page.locator('.calendar-list').waitFor({ state: 'attached' });

      // The calendar that came back is alive: its entries still open the menu,
      // and Escape still closes the menu rather than the modal.
      await page.locator('.calendar-entry.layer-override').first().click();
      await expect(page.locator('.override-context-menu.active')).toBeAttached();
      await page.keyboard.press('Escape');
      await expect(page.locator('.override-context-menu.active')).toHaveCount(0);
      await expect(page.locator('.calendar-list')).toBeAttached();

      // As is the dispatcher, which was never part of either session.
      await openOverrideModal(page);
      await expect(page.locator('#override-form')).toBeAttached();
    });

  test('a request that outlives its modal writes nothing', async ({ page }) => {
    await page.evaluate(() => { (window as any).__h.holdOverrides = true; });

    // A opens and hangs on its load, before it has touched the DOM at all.
    await page.locator('.create-override-btn').click();
    await page.waitForFunction(() => (window as any).__h.releaseOverrides !== null);

    // A is closed, and B takes the screen.
    await page.locator('#modal-close').click();
    await page.evaluate(() => { (window as any).__h.holdOverrides = false; });
    await page.locator('.edit-schedule-btn').click();
    await page.locator('#schedule-form').waitFor({ state: 'attached' });

    // Only now does A's request come back.
    await page.evaluate(() => (window as any).__h.releaseOverrides());
    await page.waitForTimeout(100);

    await expect(page.locator('#schedule-form'),
      'the editor that replaced it is still what is on screen').toBeAttached();
    await expect(page.locator('#override-form'),
      'the abandoned load rewrote the modal it no longer owns').toHaveCount(0);

    const aborted = await page.evaluate(() =>
      (window as any).__h.calls.overrideSignals.map((signal: any) => signal ? signal.aborted : null));
    expect(aborted[0],
      'the signal reached the request, so a real fetch would have been cancelled').toBe(true);
  });

  test('a reopened modal does not remember what the last one was editing', async ({ page }) => {
    await openOverrideModal(page);

    // Editing an existing override: the form now points at a revision.
    await page.locator('.edit-override-btn').first().click();
    await expect(page.locator('#modal-footer button[type="submit"]')).toHaveText('Save Changes');

    await page.locator('#modal-close').click();
    await openOverrideModal(page);

    await expect(page.locator('#modal-footer button[type="submit"]'),
      'the next open starts as a create, whatever the last one was doing')
      .toHaveText('Create Override');

    await page.locator('#override-user').selectOption('u2');
    await page.locator('#modal-footer button[type="submit"]').click();
    await page.waitForFunction(() => {
      const calls = (window as any).__h.calls;
      return calls.created > 0 || calls.updated > 0;
    });

    const calls = await page.evaluate(() => (window as any).__h.calls);
    expect(calls.created, 'it created').toBe(1);
    expect(calls.updated, 'an edit that leaked would have appended a revision').toBe(0);
  });
});
