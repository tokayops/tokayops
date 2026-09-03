import { test, expect } from '@playwright/test';

/**
 * Who wrote a journal line, as the deliveries module labels it, in the
 * browser that runs it: a person through the users directory - and "Deleted
 * user" once the directory no longer knows them - a component by its label,
 * and a line a build before this one wrote as the text it wrote, marked. The
 * kind decides, never the text: a legacy line whose actor happens to read
 * "engine" is not the engine.
 */
const MODULE = '/js/modules/deliveries.js';

test.describe('delivery actors', () => {
  test.beforeEach(async ({ page }) => {
    await page.route('**/__delivery-actors-harness', route =>
      route.fulfill({ contentType: 'text/html', body: '<!doctype html><title>harness</title><div id="root"></div>' }));
    await page.goto('/__delivery-actors-harness');
  });

  test('labels by actor_kind and names people through the directory', async ({ page }) => {
    const result = await page.evaluate(async (module: string) => {
      // The directory knows u1 and not the person who was erased.
      (window as any).API = {
        users: {
          resolve: async (ids: string[]) => ({
            users: ids.filter(id => id === 'u1').map(id => ({ id, name: 'Nina Petrova' })),
          }),
        },
      };
      const mod = await import(module);
      const root = document.getElementById('root')!;
      root.innerHTML = [
        mod.actorLabel({ actor: 'u1', actor_kind: 'user' }),
        mod.actorLabel({ actor: 'gone', actor_kind: 'user' }),
        mod.actorLabel({ actor: 'engine', actor_kind: 'system' }),
        mod.actorLabel({ actor: 'engine', actor_kind: 'legacy' }),
        mod.actorLabel({ actor: 'system', actor_kind: 'legacy' }),
        mod.targetLabel('user', 'gone'),
        mod.targetLabel('channel', 'C_ONCALL'),
      ].map(html => `<div class="line">${html}</div>`).join('');
      await mod.hydrateUserNames(root);
      return Array.from(root.querySelectorAll('.line')).map(line => ({
        text: line.textContent!.replace(/\s+/g, ' ').trim(),
        classes: (line.firstElementChild as HTMLElement).className,
        erased: Boolean(line.querySelector('.is-erased')),
      }));
    }, MODULE);

    expect(result).toEqual([
      { text: 'Nina Petrova', classes: 'journal-actor journal-actor-user', erased: false },
      { text: 'Deleted user', classes: 'journal-actor journal-actor-user is-erased', erased: true },
      { text: 'Escalation engine', classes: 'journal-actor journal-actor-system', erased: false },
      { text: 'engine legacy', classes: 'journal-actor journal-actor-legacy', erased: false },
      { text: 'system legacy', classes: 'journal-actor journal-actor-legacy', erased: false },
      { text: 'Deleted user', classes: 'delivery-target is-erased', erased: true },
      { text: 'C_ONCALL', classes: 'delivery-target', erased: false },
    ]);
  });

  test('offers the decisions of the status, and no other', async ({ page }) => {
    const result = await page.evaluate(async (module: string) => {
      (window as any).API = { users: { resolve: async () => ({ users: [] }) } };
      const mod = await import(module);
      const offered = (status: string) => {
        const root = document.getElementById('root')!;
        root.innerHTML = mod.decisionForm({ id: 'd-1', status, provider: 'slack', target_kind: 'user', target_ref: 'u1' });
        return {
          decisions: Array.from(root.querySelectorAll('input[name="decision"]')).map(el => (el as HTMLInputElement).value),
          deadline: Boolean(root.querySelector('#decision-deadline')),
        };
      };
      return {
        manual_review: offered('manual_review'),
        permanent_failed: offered('permanent_failed'),
        expired: offered('expired'),
      };
    }, MODULE);

    expect(result.manual_review).toEqual({
      decisions: ['assume_accepted', 'cancel', 'retry_current_generation', 'retry_new_generation'], deadline: false,
    });
    expect(result.permanent_failed).toEqual({
      decisions: ['cancel', 'retry_current_generation', 'retry_new_generation'], deadline: false,
    });
    expect(result.expired).toEqual({
      decisions: ['retry_current_generation', 'retry_new_generation'], deadline: true,
    });
  });
});
