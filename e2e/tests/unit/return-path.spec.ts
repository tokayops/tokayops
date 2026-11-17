import { test, expect } from '@playwright/test';

/**
 * The route kept across a sign-in, exercised in the browser that navigates.
 *
 * The value is a Location header and a page address. A check that only reads
 * the source cannot see decodeURIComponent throw, or sessionStorage keep a
 * route the address no longer shows.
 */

const MODULE = '/js/core/return-path.js';

test.describe('return path', () => {
  test.beforeEach(async ({ page }) => {
    await page.route('**/__return-path-harness', route =>
      route.fulfill({ contentType: 'text/html', body: '<!doctype html><title>harness</title>' }));
    await page.goto('/__return-path-harness');
  });

  test('a hash route is kept and a foreign target is refused', async ({ page }) => {
    const accepted = await page.evaluate(async (module: string) => {
      const mod = await import(module);
      return [
        '#/ops/alert-groups/ag-1',
        '#/cfg/teams',
        '#/ops/oncall',
      ].map(hash => mod.safeReturnHash(hash));
    }, MODULE);
    expect(accepted).toEqual([
      '#/ops/alert-groups/ag-1',
      '#/cfg/teams',
      '#/ops/oncall',
    ]);

    const refused = await page.evaluate(async (module: string) => {
      const mod = await import(module);
      return [
        'https://evil.example/phish',
        '//evil.example',
        '/\\evil.example',
        '#/ops/https://evil.example',
        '#/ops/alert-groups/ag-1%0aSet-Cookie:%20x',
        'javascript:alert(1)',
      ].map(hash => mod.safeReturnHash(hash));
    }, MODULE);
    expect(refused).toEqual(refused.map(() => ''));
  });

  test('the open route is what the login page is asked to restore', async ({ page }) => {
    const sent = await page.evaluate(async (module: string) => {
      const mod = await import(module);
      window.location.hash = '#/ops/alert-groups/ag-1';
      const login = mod.loginLocation();
      return {
        login,
        stored: sessionStorage.getItem('tokay.return'),
        sso: mod.oidcRedirectLocation('#/ops/alert-groups/ag-1'),
      };
    }, MODULE);
    expect(sent.login).toBe('/login.html?next=' + encodeURIComponent('#/ops/alert-groups/ag-1'));
    expect(sent.stored).toBe('#/ops/alert-groups/ag-1');
    expect(sent.sso).toBe('/api/auth/oidc/redirect?next=' + encodeURIComponent('#/ops/alert-groups/ag-1'));
  });

  test('a rejected next is not replaced by an older stored route', async ({ page }) => {
    const home = await page.evaluate(async (module: string) => {
      const mod = await import(module);
      sessionStorage.setItem('tokay.return', '#/ops/alert-groups/ag-1');
      history.replaceState(null, '', '/__return-path-harness?next=' + encodeURIComponent('https://evil.example/phish'));
      const kept = mod.currentReturnPath();
      return { kept, stored: sessionStorage.getItem('tokay.return'), home: mod.homeWithHash(kept) };
    }, MODULE);
    expect(home.kept).toBe('');
    expect(home.stored).toBeNull();
    expect(home.home).toBe('/');
  });

  test('an empty address is filled from the route kept in the tab', async ({ page }) => {
    const hash = await page.evaluate(async (module: string) => {
      const mod = await import(module);
      sessionStorage.setItem('tokay.return', '#/ops/alert-groups/ag-1');
      history.replaceState(null, '', '/__return-path-harness');
      mod.restoreReturnPath();
      return { hash: window.location.hash, stored: sessionStorage.getItem('tokay.return') };
    }, MODULE);
    expect(hash.hash).toBe('#/ops/alert-groups/ag-1');
    expect(hash.stored).toBeNull();
  });
});
