import { test, expect } from '@playwright/test';

/**
 * Name resolution, exercised in the browser that runs it.
 *
 * The module has two jobs that are easy to conflate: coalescing lookups made
 * in the same tick, and not asking twice for something already on its way.
 * Only the first was implemented at one point, which looked like batching and
 * still issued a request per caller whenever the network was slower than the
 * microtask queue - which it always is.
 */

const MODULE = '/js/core/users-directory.js';

test.describe('users-directory', () => {
  test.beforeEach(async ({ page }) => {
    await page.route('**/__users-directory-harness', route =>
      route.fulfill({ contentType: 'text/html', body: '<!doctype html><title>harness</title>' }));
    await page.goto('/__users-directory-harness');
  });

  test('lookups in the same tick make one request', async ({ page }) => {
    const result = await page.evaluate(async (module: string) => {
      const calls: string[][] = [];
      (window as any).API = {
        users: {
          resolve: async (ids: string[]) => {
            calls.push([...ids]);
            return { users: ids.map(id => ({ id, name: `Name ${id}` })) };
          },
        },
      };
      const mod = await import(module);
      const [a, b] = await Promise.all([
        mod.resolveNames(['u1', 'u2']),
        mod.resolveNames(['u2', 'u3']),
      ]);
      return {
        calls,
        a: [...a.entries()],
        b: [...b.entries()],
      };
    }, MODULE);

    expect(result.calls, 'two renderers in one tick, one request').toHaveLength(1);
    expect([...result.calls[0]].sort()).toEqual(['u1', 'u2', 'u3']);
    expect(result.a).toEqual([['u1', 'Name u1'], ['u2', 'Name u2']]);
    expect(result.b).toEqual([['u2', 'Name u2'], ['u3', 'Name u3']]);
  });

  test('a lookup that arrives while a request is in flight joins it', async ({ page }) => {
    const result = await page.evaluate(async (module: string) => {
      const calls: string[][] = [];
      let release: (v: unknown) => void = () => {};
      const held = new Promise(resolve => { release = resolve; });

      (window as any).API = {
        users: {
          resolve: async (ids: string[]) => {
            calls.push([...ids]);
            await held;
            return { users: ids.map(id => ({ id, name: `Name ${id}` })) };
          },
        },
      };
      const mod = await import(module);

      // First caller dispatches and blocks on the network.
      const first = mod.resolveNames(['u1', 'u2']);
      // Let the batch flush, so the request is genuinely in flight - this is
      // the state a later tick arrives in, and where a batch-only
      // implementation would ask for u2 all over again.
      await new Promise(resolve => setTimeout(resolve, 0));
      const second = mod.resolveNames(['u2']);

      release(null);
      const [a, b] = await Promise.all([first, second]);
      return { calls, a: [...a.entries()], b: [...b.entries()] };
    }, MODULE);

    expect(result.calls, 'the second caller joined the request in flight').toHaveLength(1);
    expect(result.b).toEqual([['u2', 'Name u2']]);
  });

  test('an id the server does not know is remembered as unknown', async ({ page }) => {
    const result = await page.evaluate(async (module: string) => {
      const calls: string[][] = [];
      (window as any).API = {
        users: {
          resolve: async (ids: string[]) => {
            calls.push([...ids]);
            // 'ghost' is deliberately absent from the answer.
            return { users: ids.filter(id => id !== 'ghost').map(id => ({ id, name: `Name ${id}` })) };
          },
        },
      };
      const mod = await import(module);

      const first = await mod.resolveNames(['ghost']);
      const second = await mod.resolveNames(['ghost']);
      return { calls, first: [...first.entries()], second: [...second.entries()] };
    }, MODULE);

    expect(result.first, 'an unknown id is absent from the map, not null').toEqual([]);
    expect(result.calls, 'and it is not asked for again').toHaveLength(1);
    expect(result.second).toEqual([]);
  });

  test('a failed lookup is retried rather than cached as unknown', async ({ page }) => {
    const result = await page.evaluate(async (module: string) => {
      let attempts = 0;
      (window as any).API = {
        users: {
          resolve: async (ids: string[]) => {
            attempts++;
            if (attempts === 1) throw new Error('network');
            return { users: ids.map(id => ({ id, name: `Name ${id}` })) };
          },
        },
      };
      const mod = await import(module);

      const first = await mod.resolveNames(['u9']);
      const second = await mod.resolveNames(['u9']);
      return { attempts, first: [...first.entries()], second: [...second.entries()] };
    }, MODULE);

    expect(result.first).toEqual([]);
    expect(result.attempts, 'a failure is not evidence the person is unknown').toBe(2);
    expect(result.second).toEqual([['u9', 'Name u9']]);
  });
});
