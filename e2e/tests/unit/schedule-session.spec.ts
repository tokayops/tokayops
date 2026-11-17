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

/**
 * The modules are served by the app, so they are imported through variables.
 * A literal specifier would send the type-checker looking for files that are
 * not part of this project, and it would be right: they are not.
 */
const MODULES = {
  utils: '/js/core/utils.js',
  permissions: '/js/modules/permissions.js',
  schedules: '/js/modules/schedules.js',
  session: '/js/core/modal-session.js',
};

type Listener = { type: string; aborted: boolean };

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

  await page.evaluate(async (modules: typeof MODULES) => {
    const w = window as any;

    const harness: any = {
      listeners: [] as any[],
      calls: { created: 0, updated: 0, deleted: 0, saved: 0, overrideSignals: [] as any[] },
      holdOverrides: false,
      releaseOverrides: null as any,
      // Writes are never aborted when a modal closes, so they are the ones
      // that can come back to a screen that has moved on.
      holdWrites: false,
      releaseWrite: null as any,
      failSave: false,
      failUpdate: false,
      failMembers: false,
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
        members: async () => {
          if (harness.failMembers) throw new Error('members unavailable');
          return { users: [{ id: 'u1', name: 'Ann' }, { id: 'u2', name: 'Ben' }] };
        },
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
        preview: async () => ({
          base_version: 1,
          on_call_changed: false,
          on_call_before: { l1: { user_ids: ['u1'] } },
          on_call_after: { l1: { user_ids: ['u1'] } },
          entries: [],
          warnings: [],
          evaluated_at: new Date().toISOString(),
        }),
        saveConfig: async () => {
          harness.calls.saved += 1;
          if (harness.holdWrites) {
            await new Promise(resolve => { harness.releaseWrite = resolve; });
          }
          if (harness.failSave) {
            throw Object.assign(new Error('someone else saved it'), {
              status: 409, code: 'schedule_version_conflict', body: {},
            });
          }
          return { noop: false };
        },
        createOverride: async () => {
          harness.calls.created += 1;
          if (harness.holdWrites) {
            await new Promise(resolve => { harness.releaseWrite = resolve; });
          }
          return {};
        },
        updateOverride: async () => {
          harness.calls.updated += 1;
          if (harness.holdWrites) {
            await new Promise(resolve => { harness.releaseWrite = resolve; });
          }
          if (harness.failUpdate) {
            throw Object.assign(new Error('someone else changed it'), {
              status: 409, code: 'override_revision_conflict', body: {},
            });
          }
          return {};
        },
        deleteOverride: async () => { harness.calls.deleted += 1; return {}; },
      },
    };

    const utils = await import(modules.utils);
    utils.initElements();
    const permissions = await import(modules.permissions);
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

    const schedules = await import(modules.schedules);
    schedules.bindScheduleEvents();
    harness.mark = harness.listeners.length;

    document.getElementById('page')!.innerHTML = `
      <div class="on-call-widget" data-team-id="team-1">
        <button class="create-override-btn" data-team-id="team-1">Override</button>
        <button class="view-schedule-btn" data-team-id="team-1">View</button>
        <button class="edit-schedule-btn" data-team-id="team-1">Configure</button>
      </div>`;
  }, MODULES);
}

/** What the schedule modules registered while a modal was open, and its fate. */
async function sessionListeners(page: Page): Promise<{ session: Listener[]; page: Listener[] }> {
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

async function openEditor(page: Page) {
  await page.locator('.edit-schedule-btn').click();
  await page.locator('#schedule-form').waitFor({ state: 'attached' });
}

/** Hold the next write open, and hand back the release. */
async function holdWrites(page: Page) {
  await page.evaluate(() => { (window as any).__h.holdWrites = true; });
  return async () => {
    await page.waitForFunction(() => (window as any).__h.releaseWrite !== null);
    await page.evaluate(() => (window as any).__h.releaseWrite());
    await page.waitForTimeout(100);
  };
}

async function waitForToast(page: Page) {
  await page.waitForFunction(() =>
    document.querySelectorAll('#toast-container .toast').length > 0);
}

/**
 * A schedule with someone else to hand to, and a note of every save.
 *
 * The review step only offers the choice when L1 has more than one group.
 * The harness schedule has one, which is why these tests replace it.
 */
async function useTwoGroupSchedule(page: Page) {
  await page.evaluate(() => {
    const harness = (window as any).__h;
    harness.saves = [] as unknown[];
    harness.holdPreview = false;
    harness.releasePreview = null;
    harness.previewSettled = 0;

    (window as any).API.schedules.getConfig = async () => ({
      schedule_id: 'sch-1',
      version: 1,
      config: {
        timezone: 'UTC',
        slack_usergroup_id: '',
        l1: {
          enabled: true,
          rotation_type: 'daily',
          handoff_time: '09:00',
          handoff_day: null,
          groups: [
            { id: 'g1', user_ids: ['u1'] },
            { id: 'g2', user_ids: ['u2'] },
          ],
        },
        l2: {
          enabled: false,
          escalation_timeout_minutes: 5,
          rotation_type: 'daily',
          handoff_time: '09:00',
          handoff_day: null,
          user_ids: [],
        },
      },
    });

    const onCall = {
      user_ids: ['u1'],
      group_id: 'g1',
      source: 'rotation',
      assignment_end: '2026-11-23T08:00:00Z',
      grid_slot_end: '2026-11-23T08:00:00Z',
    };
    (window as any).API.schedules.preview = async (
      _teamId: string, _config: unknown, _until: unknown, options: any = {},
    ) => {
      // Only the handoff re-preview waits. Holding the first one would mean
      // the review step never opened.
      if (options.handoffNowGroupId && harness.holdPreview) {
        await new Promise<void>(resolve => { harness.releasePreview = resolve; });
        harness.previewSettled += 1;
      }
      return {
        base_version: 1,
        on_call_changed: false,
        on_call_before: { l1: onCall },
        on_call_after: { l1: onCall },
        entries: [],
        warnings: [],
        evaluated_at: new Date().toISOString(),
      };
    };

    const save = (window as any).API.schedules.saveConfig;
    (window as any).API.schedules.saveConfig = async (...args: unknown[]) => {
      harness.saves.push(args);
      return save(...args);
    };
  });
}

/** A click Playwright will not send, because the button is disabled. */
async function syntheticClick(page: Page, selector: string) {
  await page.evaluate((sel) => {
    document.querySelector(sel)!.dispatchEvent(new MouseEvent('click', { bubbles: true }));
  }, selector);
}

test.describe('modal session', () => {
  test.beforeEach(async ({ page }) => {
    await boot(page);
  });

  test('close() is idempotent, and aborts once', async ({ page }) => {
    const result = await page.evaluate(async (module: string) => {
      const { beginModalSession } = await import(module);
      const session = beginModalSession();

      let aborts = 0;
      session.signal.addEventListener('abort', () => { aborts += 1; });

      session.close();
      session.close();
      session.closeModal();

      return { aborts, closed: session.closed };
    }, MODULES.session);

    // Four ways out of a modal run into each other: cancel closes and then
    // Escape arrives, or the X is clicked twice. The second one through has to
    // be a no-op, not a second teardown.
    expect(result.aborts, 'one open, one teardown').toBe(1);
    expect(result.closed).toBe(true);
  });

  test('a new modal starts a session the old one cannot reach', async ({ page }) => {
    const result = await page.evaluate(async (module: string) => {
      const { beginModalSession } = await import(module);
      const first = beginModalSession();
      const second = beginModalSession();
      return { first: first.closed, second: second.closed };
    }, MODULES.session);

    expect(result.first, 'opening a modal ends the one it replaced').toBe(true);
    expect(result.second).toBe(false);
  });

  test('a session that is no longer on screen cannot close the shell', async ({ page }) => {
    const result = await page.evaluate(async (module: string) => {
      const { beginModalSession } = await import(module);
      const overlay = document.getElementById('modal-overlay')!;

      const first = beginModalSession();
      overlay.classList.add('active');
      // Whatever opened next took the screen, and with it the right to close
      // it. `first` is what a write still holds when it finishes late.
      const second = beginModalSession();
      overlay.classList.add('active');

      first.closeModal();
      const afterStale = overlay.classList.contains('active');
      second.closeModal();

      return { afterStale, afterOwner: overlay.classList.contains('active') };
    }, MODULES.session);

    expect(result.afterStale,
      'a modal nobody is looking at closed the one they are').toBe(true);
    expect(result.afterOwner, 'and the session on screen still closes it').toBe(false);
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

  test('a save that lands after its modal was replaced leaves the new one alone',
    async ({ page }) => {
      await openEditor(page);
      const release = await holdWrites(page);

      await page.locator('#schedule-form-submit').click();
      await page.locator('#preview-confirm').waitFor({ state: 'attached' });
      await page.locator('#preview-confirm').click();

      // The editor is dismissed while its save is still in flight, and an
      // override modal takes the screen.
      await page.locator('#modal-close').click();
      await openOverrideModal(page);

      await release();

      // The save is not cancelled - it had already changed something - but the
      // modal it belonged to is gone, and closing "its" modal now would close
      // this one.
      await expect(page.locator('#modal-overlay'),
        'the save closed the modal that replaced it').toHaveClass(/active/);
      await expect(page.locator('#override-form')).toBeAttached();
    });

  test('a refused save does not reopen its editor over the modal that replaced it',
    async ({ page }) => {
      await openEditor(page);
      await page.evaluate(() => { (window as any).__h.failSave = true; });
      const release = await holdWrites(page);

      await page.locator('#schedule-form-submit').click();
      await page.locator('#preview-confirm').waitFor({ state: 'attached' });
      await page.locator('#preview-confirm').click();

      await page.locator('#modal-close').click();
      await openOverrideModal(page);

      await release();

      // A version conflict reopens the editor, which is right while the editor
      // is on screen and is somebody else's modal once it is not.
      await expect(page.locator('#schedule-form'),
        'the refusal reopened an editor over the override modal').toHaveCount(0);
      await expect(page.locator('#override-form')).toBeAttached();
    });

  test('an override created after its modal was replaced leaves the new one alone',
    async ({ page }) => {
      await openOverrideModal(page);
      await page.locator('#override-user').selectOption('u2');
      const release = await holdWrites(page);

      await page.locator('#modal-footer button[type="submit"]').click();
      await page.locator('#modal-close').click();
      await openEditor(page);

      await release();

      await expect(page.locator('#modal-overlay')).toHaveClass(/active/);
      await expect(page.locator('#schedule-form')).toBeAttached();
      // resetForm() reaches for the footer by shape, not by ownership, so a
      // create finishing late used to relabel whatever button was there.
      await expect(page.locator('#modal-footer button[type="submit"]'),
        'the finished create rewrote the new modal\'s footer').toHaveText(/Review changes/);
    });

  test('a refused update does not recover the form of the modal that replaced it',
    async ({ page }) => {
      const submit = page.locator('#modal-footer button[type="submit"]');

      await openOverrideModal(page);
      await page.locator('.edit-override-btn').first().click();
      await expect(submit).toHaveText('Save Changes');

      await page.evaluate(() => { (window as any).__h.failUpdate = true; });
      const release = await holdWrites(page);
      await submit.click();

      // A is dismissed mid-write; B opens and is editing an override of its
      // own by the time A's refusal arrives.
      await page.locator('#modal-close').click();
      await openOverrideModal(page);
      await page.locator('.edit-override-btn').first().click();
      await expect(submit).toHaveText('Save Changes');

      await release();

      // Recovering A's form here would put B's form back to "create" while
      // B's session went on editing - the screen and the session disagreeing
      // about what the next submit does.
      await expect(submit,
        'the refusal reset the form of a modal that was editing').toHaveText('Save Changes');
      await expect(page.locator('.override-form-title')).toContainText('Edit Override');
    });

  test('a modal that could not load leaves no listeners behind', async ({ page }) => {
    await page.evaluate(() => { (window as any).__h.failMembers = true; });
    await page.locator('.create-override-btn').click();
    await waitForToast(page);

    const listeners = await sessionListeners(page);
    expect(listeners.session.filter(l => !l.aborted),
      'a modal that never appeared still owns the keyboard').toEqual([]);
    expect(listeners.page.filter(l => l.aborted),
      'and it took the dispatcher with it').toEqual([]);
  });

  test('an override that fails to load from the calendar hands the calendar back',
    async ({ page }) => {
      await page.locator('.view-schedule-btn').click();
      await page.locator('.calendar-list').waitFor({ state: 'attached' });
      await page.locator('.calendar-entry.layer-override').first().click();
      await expect(page.locator('.override-context-menu.active')).toBeAttached();

      await page.evaluate(() => { (window as any).__h.failMembers = true; });
      await page.locator('.override-context-menu-item[data-action="edit"]').click();
      await waitForToast(page);

      // The calendar was already on screen when the override modal took over,
      // so failing to load leaves a picture with nothing behind it unless the
      // way out is the ordinary one.
      await page.locator('.calendar-list').waitFor({ state: 'attached' });
      await page.locator('.calendar-entry.layer-override').first().click();
      await expect(page.locator('.override-context-menu.active'),
        'the calendar came back dead').toBeAttached();
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

  test('a second save cannot leave while the first is still in flight', async ({ page }) => {
    await useTwoGroupSchedule(page);
    const release = await holdWrites(page);

    await openEditor(page);
    await page.locator('#schedule-form-submit').click();
    await page.locator('input[name="handoff-mode"][value="now"]').check();
    // The re-preview enables Save again once it has an answer. The click
    // waits for that, and then the write is what we hold.
    await page.locator('#preview-confirm').click();
    await page.waitForFunction(() => (window as any).__h.releaseWrite !== null);

    // The fieldset carries disabled, which is what turns the radios and the
    // list off. Playwright does not report the fieldset itself as disabled.
    await expect(page.locator('.handoff-choice')).toHaveAttribute('disabled', '');
    await expect(page.locator('input[name="handoff-mode"]')).toHaveCount(2);
    for (const control of [
      page.locator('input[name="handoff-mode"]').nth(0),
      page.locator('input[name="handoff-mode"]').nth(1),
      page.locator('#handoff-group'),
      page.locator('#preview-confirm'),
    ]) {
      await expect(control).toBeDisabled();
    }
    const inflight = await page.evaluate(() => (window as any).__h.saves.map((args: any[]) => args[4]));
    expect(inflight, 'the write captured the handoff that was chosen').toEqual(['g2']);

    // The controls are disabled, so a person cannot do this. Dispatching it
    // anyway is what reaches the guard: a Playwright click on a disabled
    // button never fires, and neither does HTMLElement.click().
    await page.evaluate(() => {
      const keep = document.querySelector('input[name="handoff-mode"][value="keep"]') as HTMLInputElement;
      keep.checked = true;
      keep.dispatchEvent(new Event('change', { bubbles: true }));
    });
    await syntheticClick(page, '#preview-confirm');

    const during = await page.evaluate(() => (window as any).__h.saves.map((args: any[]) => args[4]));
    expect(during, 'neither the switch nor the second click started a save').toEqual(['g2']);
    await expect(page.locator('#preview-confirm')).toBeDisabled();

    await release();
    expect(await page.evaluate(() => (window as any).__h.saves.length)).toBe(1);
  });

  test('a preview that answers after Save was pressed does not unlock it', async ({ page }) => {
    await useTwoGroupSchedule(page);
    await page.evaluate(() => { (window as any).__h.holdPreview = true; });
    const release = await holdWrites(page);

    await openEditor(page);
    await page.locator('#schedule-form-submit').click();
    await page.locator('input[name="handoff-mode"][value="now"]').check();
    await page.waitForFunction(() => (window as any).__h.releasePreview !== null);
    await expect(page.locator('#preview-confirm')).toBeDisabled();

    // Save is disabled because the preview has not answered. The click is
    // synthetic so the write starts anyway, and the preview is still held.
    await syntheticClick(page, '#preview-confirm');
    await page.waitForFunction(() => (window as any).__h.releaseWrite !== null);
    expect(await page.evaluate(() => (window as any).__h.saves.length)).toBe(1);

    // setTimeout yields past the preview's microtask, so the assertion sees
    // the button after that answer has been applied, not before it.
    await page.evaluate(() => new Promise((resolve) => {
      (window as any).__h.releasePreview();
      setTimeout(resolve, 0);
    }));
    expect(await page.evaluate(() => (window as any).__h.previewSettled)).toBe(1);

    await expect(page.locator('#preview-confirm'),
      'the late preview re-enabled Save over the write').toBeDisabled();
    expect(await page.evaluate(() => (window as any).__h.saves.length)).toBe(1);

    await release();
    expect(await page.evaluate(() => (window as any).__h.saves.length)).toBe(1);
  });

  test('a refused save unlocks the handoff choice', async ({ page }) => {
    await useTwoGroupSchedule(page);
    await page.evaluate(() => {
      const harness = (window as any).__h;
      (window as any).API.schedules.saveConfig = async (...args: unknown[]) => {
        harness.saves.push(args);
        throw Object.assign(new Error('no such group'), {
          status: 422,
          code: 'validation_failed',
          body: { field: 'l1_handoff_now_group_id' },
        });
      };
    });

    await openEditor(page);
    await page.locator('#schedule-form-submit').click();
    await page.locator('#preview-confirm').waitFor({ state: 'attached' });
    await page.locator('#preview-confirm').click();

    // The refusal leaves the review step up. The choice has to work again,
    // or the only way out is to reopen the editor.
    await expect(page.locator('#preview-confirm')).toBeEnabled();
    await expect(page.locator('.handoff-choice')).not.toHaveAttribute('disabled', '');
    await expect(page.locator('#handoff-group')).toBeEnabled();
    expect(await page.evaluate(() => (window as any).__h.saves.length)).toBe(1);

    await page.locator('#preview-confirm').click();
    await page.waitForFunction(() => (window as any).__h.saves.length === 2);
    await expect(page.locator('#handoff-group')).toBeEnabled();
  });
});
