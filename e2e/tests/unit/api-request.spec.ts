import { test, expect, Page } from '@playwright/test';

/**
 * The request wrapper every module calls, exercised in the browser that runs
 * it: what reaches the server is what the wrapper composed, and the
 * composition is where a production-only refusal hid.
 *
 * CSRF protection is on in production and off in this environment, so no flow
 * through the real server can notice a request that lost its token - the replay
 * used to, and every end-to-end test of it stayed green. The headers are
 * asserted at the network boundary instead, with the server's answer stubbed,
 * in each browser the suite runs.
 */
const MODULE = '/js/api.js';

type Headers = Record<string, string>;

/**
 * Runs one call of the API module in the page and returns the headers the
 * browser actually sent for it. The endpoint is stubbed, so nothing depends on
 * the server's state or on what the call means to it.
 */
async function headersSentBy(
  page: Page,
  endpoint: string,
  call: string,
): Promise<Headers> {
  let sent: Headers | null = null;
  await page.route(`**${endpoint}`, route => {
    sent = route.request().headers();
    return route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ ok: true, message: 'stubbed', delivery_id: 'del-2' }),
    });
  });
  await page.evaluate(async (args: { module: string; call: string }) => {
    await import(args.module);
    // The call is a small expression over window.API, evaluated where the
    // module put it.
    await new Function('API', `return (${args.call});`)((window as any).API);
  }, { module: MODULE, call });
  await page.unroute(`**${endpoint}`);
  if (!sent) {
    throw new Error(`${call} sent nothing to ${endpoint}`);
  }
  return sent;
}

test.describe('api request wrapper', () => {
  // A blank page on the app's origin, with the CSRF cookie the middleware
  // would have set: the wrapper reads the token from document.cookie.
  test.beforeEach(async ({ page }) => {
    await page.route('**/__api-request-harness', route =>
      route.fulfill({ contentType: 'text/html', body: '<!doctype html><title>harness</title>' }));
    await page.goto('/__api-request-harness');
    await page.evaluate(() => {
      document.cookie = '_csrf=e2e-csrf-token; path=/';
    });
  });

  test('a replay carries the CSRF token beside its idempotency key', async ({ page }) => {
    const sent = await headersSentBy(page,
      '/api/v1/integrations/int-1/deliveries/del-1/replay',
      `API.integrations.replayDelivery('int-1', 'del-1', 'press-1')`);
    // The three together: the caller's own header, the token the middleware
    // demands, and the content type every request declares.
    expect(sent['idempotency-key']).toBe('press-1');
    expect(sent['x-csrf-token']).toBe('e2e-csrf-token');
    expect(sent['content-type']).toBe('application/json');
  });

  test('a mutating request without headers of its own carries the token too', async ({ page }) => {
    const sent = await headersSentBy(page,
      '/api/v1/integrations/int-1',
      `API.integrations.delete('int-1')`);
    expect(sent['x-csrf-token']).toBe('e2e-csrf-token');
    expect(sent['content-type']).toBe('application/json');
  });

  test('a read sends no token', async ({ page }) => {
    const sent = await headersSentBy(page,
      '/api/v1/integrations/int-1/deliveries',
      `API.integrations.deliveries('int-1')`);
    expect(sent['x-csrf-token']).toBeUndefined();
  });
});
