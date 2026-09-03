import { test, expect } from '@playwright/test';

/**
 * The deliveries module's rendering, in the browser that runs it.
 *
 * Who wrote a journal line is labelled by actor_kind: a person through the
 * users directory - which answers "Deleted user" for somebody erased, and
 * nothing at all for an id it has never known - a component by its label, and
 * a line a build before this one wrote as the text it wrote, marked. The kind
 * decides, never the text: a legacy line whose actor reads "engine" is not
 * the engine.
 *
 * Ids go into attributes, and an id is whatever a user was created with. A
 * quote in one must stay inside the attribute; escaped as text it would not.
 */
const MODULE = '/js/modules/deliveries.js';

// A user id nobody should have, and the store accepts: it closes the
// attribute and opens another if the render escapes it as text.
const HOSTILE = 'x" onmouseover="document.title=\'pwned\'';

test.describe('delivery actors', () => {
  test.beforeEach(async ({ page }) => {
    await page.route('**/__delivery-actors-harness', route =>
      route.fulfill({
        contentType: 'text/html',
        body: '<!doctype html><title>harness</title><script src="/js/components.js"></script><div id="root"></div>',
      }));
    await page.goto('/__delivery-actors-harness');
  });

  test('labels by actor_kind and names people through the directory', async ({ page }) => {
    const result = await page.evaluate(async (module: string) => {
      // The directory knows u1, answers for the erased person under the name
      // erasure left them, and has never heard of "gone".
      (window as any).API = {
        users: {
          resolve: async (ids: string[]) => ({
            users: ids.flatMap(id => {
              if (id === 'u1') return [{ id, name: 'Nina Petrova' }];
              if (id === 'erased') return [{ id, name: 'Deleted user' }];
              return [];
            }),
          }),
        },
      };
      const mod = await import(module);
      const root = document.getElementById('root')!;
      root.innerHTML = [
        mod.actorLabel({ actor: 'u1', actor_kind: 'user' }),
        mod.actorLabel({ actor: 'erased', actor_kind: 'user' }),
        mod.actorLabel({ actor: 'gone', actor_kind: 'user' }),
        mod.actorLabel({ actor: 'engine', actor_kind: 'system' }),
        mod.actorLabel({ actor: 'engine', actor_kind: 'legacy' }),
        mod.actorLabel({ actor: 'system', actor_kind: 'legacy' }),
        mod.targetLabel('user', 'erased'),
        mod.targetLabel('user', 'gone'),
        mod.targetLabel('channel', 'C_ONCALL'),
      ].map(html => `<div class="line">${html}</div>`).join('');
      await mod.hydrateUserNames(root);
      return Array.from(root.querySelectorAll('.line')).map(line => ({
        text: line.textContent!.replace(/\s+/g, ' ').trim(),
        classes: (line.firstElementChild as HTMLElement).className,
      }));
    }, MODULE);

    expect(result).toEqual([
      { text: 'Nina Petrova', classes: 'journal-actor journal-actor-user' },
      { text: 'Deleted user', classes: 'journal-actor journal-actor-user' },
      { text: 'gone', classes: 'journal-actor journal-actor-user is-unknown' },
      { text: 'Escalation engine', classes: 'journal-actor journal-actor-system' },
      { text: 'engine legacy', classes: 'journal-actor journal-actor-legacy' },
      { text: 'system legacy', classes: 'journal-actor journal-actor-legacy' },
      { text: 'Deleted user', classes: 'delivery-target' },
      { text: 'gone', classes: 'delivery-target is-unknown' },
      { text: 'C_ONCALL', classes: 'delivery-target' },
    ]);
  });

  test('an id with quotes stays inside its attribute', async ({ page }) => {
    const result = await page.evaluate(async ({ module, hostile }: { module: string; hostile: string }) => {
      const asked: string[][] = [];
      (window as any).API = {
        users: {
          resolve: async (ids: string[]) => {
            asked.push([...ids]);
            return { users: ids.map(id => ({ id, name: 'Named ' + id.length })) };
          },
        },
      };
      const mod = await import(module);
      // The journal's buttons are the administrator's. The module's own
      // Permissions object is on the window once the module has loaded.
      (window as any).Permissions.user = { id: 'admin', role: 'admin' };
      const root = document.getElementById('root')!;
      const at = '2026-09-03T12:00:00Z';
      root.innerHTML = [
        mod.targetLabel('user', hostile),
        mod.actorLabel({ actor: hostile, actor_kind: 'user' }),
        (window as any).Components.deliveryTarget('user', hostile),
        (window as any).Components.timelineEvent({
          type: 'notification_failed', message: 'failed', created_at: at, actor: 'system',
          metadata: { intent_id: hostile, provider: 'slack', target_kind: 'user', target_ref: hostile },
        }),
        mod.groupDeliveriesBlock({
          paging: [{ id: hostile, status: 'permanent_failed', provider: 'slack', target_kind: 'user',
            target_ref: hostile, form: 'oneshot', created_at: at }],
          events: [{ event_id: hostile, event_type: 'alert_group.firing', status: 'fanned_out', created_at: at,
            batches: [{ batch_id: hostile, kind: 'webhook_event', outcome: 'admitted', intent_count: 1, admitted_at: at,
              deliveries: [{ id: hostile, status: 'pending', target_kind: 'subscriber', target_ref: hostile, created_at: at }] }] }],
        }),
      ].map(html => `<div class="line">${html}</div>`).join('');
      await mod.hydrateUserNames(root);
      const attrs = (selector: string, name: string) =>
        Array.from(root.querySelectorAll(selector)).map(el => el.getAttribute(name));
      return {
        title: document.title,
        handlers: root.querySelectorAll('[onmouseover]').length,
        userIds: attrs('[data-user-id]', 'data-user-id'),
        deliveryIds: attrs('[data-delivery-id]', 'data-delivery-id'),
        eventIds: attrs('[data-event-id]', 'data-event-id'),
        names: Array.from(root.querySelectorAll('.delivery-target-name')).map(el => el.textContent),
        asked,
      };
    }, { module: MODULE, hostile: HOSTILE });

    expect(result.title, 'nothing ran').toBe('harness');
    expect(result.handlers, 'no attribute was opened').toBe(0);
    // Five places name the person: the two labels, the timeline's target twice
    // (its own helper and the event), the paging row.
    expect(result.userIds, 'every user id is the id, whole').toEqual(Array(5).fill(HOSTILE));
    // Six name the delivery: the timeline line and its button, the paging row
    // and its button, the webhook row and its button.
    expect(result.deliveryIds).toEqual(Array(6).fill(HOSTILE));
    expect(result.eventIds).toEqual([HOSTILE]);
    // And the directory was asked for the id as it is, and answered for it.
    expect(result.asked.flat().every(id => id === HOSTILE)).toBe(true);
    expect(result.names.every(name => name === 'Named ' + HOSTILE.length)).toBe(true);
  });

  test('offers the decisions of the status and the family, and no other', async ({ page }) => {
    const result = await page.evaluate(async (module: string) => {
      (window as any).API = { users: { resolve: async () => ({ users: [] }) } };
      const mod = await import(module);
      const offered = (status: string, family: string) => {
        const root = document.getElementById('root')!;
        const delivery = { id: 'd-1', status, family, provider: family === 'webhook' ? 'webhook' : 'slack',
          target_kind: family === 'webhook' ? 'subscriber' : 'user', target_ref: 'u1' };
        root.innerHTML = mod.decisionForm(delivery);
        return {
          decisions: mod.decisionsFor(delivery),
          rendered: Array.from(root.querySelectorAll('input[name="decision"]')).map(el => (el as HTMLInputElement).value),
          deadline: Boolean(root.querySelector('#decision-deadline')),
          replayHint: Boolean(root.querySelector('.decision-replay-hint')),
        };
      };
      return {
        paging: {
          manual_review: offered('manual_review', 'notification'),
          permanent_failed: offered('permanent_failed', 'notification'),
          expired: offered('expired', 'handoff'),
          succeeded: offered('succeeded', 'notification'),
        },
        webhook: {
          manual_review: offered('manual_review', 'webhook'),
          permanent_failed: offered('permanent_failed', 'webhook'),
          expired: offered('expired', 'webhook'),
        },
      };
    }, MODULE);

    const all = ['assume_accepted', 'cancel', 'retry_current_generation', 'retry_new_generation'];
    expect(result.paging.manual_review).toEqual({ decisions: all, rendered: all, deadline: false, replayHint: false });
    expect(result.paging.permanent_failed).toEqual({
      decisions: ['cancel', 'retry_current_generation', 'retry_new_generation'],
      rendered: ['cancel', 'retry_current_generation', 'retry_new_generation'], deadline: false, replayHint: false,
    });
    expect(result.paging.expired).toEqual({
      decisions: ['retry_current_generation', 'retry_new_generation'],
      rendered: ['retry_current_generation', 'retry_new_generation'], deadline: true, replayHint: false,
    });
    expect(result.paging.succeeded).toEqual({ decisions: [], rendered: [], deadline: false, replayHint: false });
    // A webhook is retried by replay, never here: what is left is to
    // withdraw, or in review to assume the call landed; an expiry offers
    // nothing.
    expect(result.webhook.manual_review).toEqual({
      decisions: ['assume_accepted', 'cancel'], rendered: ['assume_accepted', 'cancel'], deadline: false, replayHint: true,
    });
    expect(result.webhook.permanent_failed).toEqual({ decisions: ['cancel'], rendered: ['cancel'], deadline: false, replayHint: true });
    expect(result.webhook.expired).toEqual({ decisions: [], rendered: [], deadline: true, replayHint: true });
  });
});
