import { Browser, Page, expect } from '@playwright/test';
import { LoginPage } from '../../pages/login.page';
import { TeamFixtures, deleteTeam } from '../../fixtures/team.fixture';

/**
 * What the delivery tests drive through the API: an alert fired into a
 * seeded team, whose paging the disabled Slack integration refuses before
 * any network call - a deterministic permanent failure - and, when asked, a
 * webhook subscriber pointing back at the app, whose 404 is a refusal too.
 */

export const BASE_URL = process.env.BASE_URL || 'http://localhost:8081';

/** The seeded Alertmanager webhook integration's secret. */
export const INBOUND_SECRET = 'e2e-test-secret';

/** The seeded password of every seeded user. */
export const SEEDED_PASSWORD = 'Admin123!';

export type Delivery = {
  id: string;
  status: string;
  family: string;
  kind: string;
  provider: string;
  target_kind: string;
  target_ref: string;
  alert_group_id?: string;
};

export type GroupDeliveries = {
  paging: Delivery[];
  events: Array<{
    event_id: string;
    event_type: string;
    status: string;
    batches: Array<{ batch_id: string; kind: string; outcome: string; intent_count: number; deliveries: Delivery[] }>;
  }>;
};

/**
 * Fire one alert for a team through the seeded inbound webhook. The group key
 * is the alert group's dedup key, which is how the group is found again.
 */
export async function fireAlert(page: Page, groupKey: string, teamId: string, alertname = 'E2E Delivery'): Promise<void> {
  const payload = {
    status: 'firing',
    groupKey,
    commonLabels: { team: teamId, severity: 'critical', alertname },
    alerts: [{
      status: 'firing',
      labels: { alertname, severity: 'critical', team: teamId },
      annotations: { summary: `${alertname} (${groupKey})` },
      fingerprint: `fp-${groupKey}`,
      startsAt: new Date().toISOString(),
      endsAt: '0001-01-01T00:00:00Z',
    }],
  };
  const resp = await page.request.post(`${BASE_URL}/webhook/alertmanager?token=${INBOUND_SECRET}`, { data: payload });
  expect(resp.status(), 'the inbound webhook accepts the alert').toBe(200);
}

export type PagedTeam = {
  teamId: string;
  userId: string;
  userName: string;
  policyId: string;
  /** Removes what was made, through the request context of the page given. */
  cleanup: (page: Page) => Promise<void>;
};

/**
 * A team whose alerts page one person over Slack. Slack is not configured on
 * this installation, so the page is refused before any network call and the
 * delivery fails for good - which is the state the operator's door is for.
 * The seeded teams carry no default policy, so the team is this test's own.
 */
export async function pagedTeam(page: Page, prefix: string): Promise<PagedTeam> {
  const fixtures = new TeamFixtures(page);
  const userId = `e2e-paged-${prefix}`;
  const teamId = await fixtures.team(`paged-${prefix}`, [userId]);
  const policy = await page.request.post(`${BASE_URL}/api/v1/policies`, {
    data: {
      name: `E2E paging ${prefix}`,
      team_id: teamId,
      steps: [{
        provider: 'slack', target_kind: 'dm', target_type: 'user', target_id: userId,
        delay_seconds: 0, timeout_seconds: 30, max_attempts: 1,
      }],
    },
  });
  expect(policy.ok(), `create the paging policy: ${policy.status()} ${await policy.text()}`).toBeTruthy();
  const policyId = (await policy.json()).id;
  const updated = await page.request.put(`${BASE_URL}/api/v1/teams/${teamId}`, {
    data: { name: `E2E paged-${prefix}`, description: '', default_policy_id: policyId },
  });
  expect(updated.ok(), `set the team's default policy: ${updated.status()} ${await updated.text()}`).toBeTruthy();
  return {
    teamId, userId, userName: userId.replace('e2e-', 'E2E '), policyId,
    cleanup: async (cleaner: Page) => {
      await cleaner.request.put(`${BASE_URL}/api/v1/teams/${teamId}`, {
        data: { name: `E2E paged-${prefix}`, description: '', default_policy_id: '' },
      });
      await cleaner.request.delete(`${BASE_URL}/api/v1/policies/${policyId}`);
      const outcome = await deleteTeam(cleaner, teamId);
      expect(outcome.result, `remove ${teamId}: ${JSON.stringify(outcome)}`).not.toBe('failed');
    },
  };
}

/** The alert group an alert made, by its dedup key. */
export async function alertGroupByKey(page: Page, groupKey: string, timeoutMs = 20000): Promise<{ id: string; team_id: string; status: string }> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const resp = await page.request.get(
      `${BASE_URL}/api/v1/alert-groups?limit=100&statuses=processing,triggered,acknowledged,resolved&sort=created_at&sort_dir=desc`,
    );
    if (resp.ok()) {
      const json = await resp.json();
      const found = (json.alert_groups || []).find((g: { dedup_key: string }) => g.dedup_key === groupKey);
      if (found) return found;
    }
    await page.waitForTimeout(500);
  }
  throw new Error(`no alert group for ${groupKey} after ${timeoutMs}ms`);
}

export async function groupDeliveries(page: Page, alertGroupId: string): Promise<GroupDeliveries> {
  const resp = await page.request.get(`${BASE_URL}/api/v1/alert-groups/${alertGroupId}/deliveries`);
  expect(resp.ok(), `deliveries of ${alertGroupId}`).toBeTruthy();
  return resp.json();
}

/**
 * Wait until the group's deliveries satisfy a condition - the worker runs on
 * its own clock.
 */
export async function untilDeliveries(
  page: Page,
  alertGroupId: string,
  what: string,
  cond: (d: GroupDeliveries) => boolean,
  timeoutMs = 30000,
): Promise<GroupDeliveries> {
  const deadline = Date.now() + timeoutMs;
  let last: GroupDeliveries | null = null;
  while (Date.now() < deadline) {
    last = await groupDeliveries(page, alertGroupId);
    if (cond(last)) return last;
    await page.waitForTimeout(1000);
  }
  throw new Error(`timed out waiting for ${what}: ${JSON.stringify(last)}`);
}

/** A page whose first delivery has failed for good, without any network. */
export async function untilPagingFailed(page: Page, alertGroupId: string): Promise<Delivery> {
  const d = await untilDeliveries(page, alertGroupId, 'the paging to fail for good',
    g => g.paging.some(p => p.status === 'permanent_failed'));
  return d.paging.find(p => p.status === 'permanent_failed')!;
}

export async function createWebhookIntegration(page: Page, name: string, url: string): Promise<string> {
  const resp = await page.request.post(`${BASE_URL}/api/v1/integrations`, {
    data: {
      type: 'generic_webhook', name, enabled: true, scope: 'global',
      config: { url, secret: 'e2e-test-secret', timeout_seconds: 2 },
    },
  });
  expect(resp.ok(), `create the webhook integration: ${resp.status()}`).toBeTruthy();
  return (await resp.json()).id;
}

export async function deleteIntegration(page: Page, id: string): Promise<void> {
  if (id) await page.request.delete(`${BASE_URL}/api/v1/integrations/${id}`);
}

export async function replayDelivery(page: Page, integrationId: string, deliveryId: string, key: string): Promise<string> {
  const resp = await page.request.post(
    `${BASE_URL}/api/v1/integrations/${integrationId}/deliveries/${deliveryId}/replay`,
    { headers: { 'Idempotency-Key': key } },
  );
  expect(resp.ok(), `replay ${deliveryId}: ${resp.status()}`).toBeTruthy();
  return (await resp.json()).delivery_id;
}

/** The person the tests run as, as the server knows them. */
export async function currentUser(page: Page): Promise<{ id: string; name: string; email: string }> {
  const resp = await page.request.get(`${BASE_URL}/api/auth/me`);
  expect(resp.ok()).toBeTruthy();
  return resp.json();
}

/**
 * A page logged in as somebody else. The caller closes the context.
 */
export async function loginAs(browser: Browser, email: string, password = SEEDED_PASSWORD): Promise<Page> {
  // A context made here gets none of the project's options, the base URL
  // included; without it a relative navigation goes nowhere.
  const context = await browser.newContext({ baseURL: BASE_URL, storageState: { cookies: [], origins: [] } });
  const page = await context.newPage();
  const login = new LoginPage(page);
  await login.goto();
  await login.login(email, password);
  await page.waitForURL(url => !url.pathname.includes('login.html'), { timeout: 15000 });
  return page;
}

/** A unique dedup key for one test. */
export function groupKey(prefix: string): string {
  return `e2e-${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
}
