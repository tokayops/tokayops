import { test, expect } from '../../fixtures/auth.fixture';
import { TeamFixtures } from '../../fixtures/team.fixture';

/**
 * The environment this suite runs against, asserted before anything is built
 * on top of it.
 *
 * `seed` still writes schedules the old way, so `e2e-up` puts the database
 * through the same destructive reset the real upgrade performs. If that step
 * is ever dropped, the suite would silently start testing the one state the
 * app no longer supports - seeded schedules reading as unconfigured, and every
 * attempt to create one refused as pre-revision. These failures name that
 * cause directly, instead of leaving it to be inferred from a dozen unrelated
 * ones.
 *
 * Nothing here writes to seeded data. A spec that configured a seeded team
 * would pass once and then fail against its own leftovers - deleting a
 * schedule is a soft delete, so the row it left behind is not the state it
 * started from.
 */
test.describe('e2e environment', () => {
  const seededTeam = 'platform';

  // Every browser project shares one server, and what this checks is a
  // property of the deployment rather than of the browser. Running it per
  // project would just assert the same fact twice.
  test.skip(({ browserName }) => browserName !== 'chromium',
    'environment invariants are browser-independent; asserted once');

  let fixtures: TeamFixtures;

  test.beforeEach(async ({ page }) => {
    fixtures = new TeamFixtures(page);
  });

  test.afterEach(async () => {
    await fixtures.cleanup();
  });

  test('the reset removed the seeded pre-revision schedules', async ({ page }) => {
    // The load-bearing check. `seed` gives this team a schedule the old way,
    // so the legacy read answers 200 for it right up until the reset deletes
    // the row. This is the one assertion that distinguishes "reset ran" from
    // "reset was skipped" - the revision endpoints answer 404 either way,
    // because a pre-revision row has no configuration in this model.
    const legacy = await page.request.get(`/api/v1/teams/${seededTeam}/schedule`);
    expect(legacy.status(),
      'a seeded pre-revision schedule survived. Either e2e-up skipped ' +
      '`migrate reset-schedules`, or it ran against a database that had ' +
      'already been reset once - the reset is a no-op after its marker exists, ' +
      'so a re-seed on a surviving volume is never cleaned up. Start from a ' +
      'fresh volume: make e2e-down && make e2e-up.')
      .toBe(404);
  });

  test('a seeded team carries no schedule and is reported unconfigured', async ({ page }) => {
    const config = await page.request.get(`/api/v1/teams/${seededTeam}/schedule/config`);
    expect(config.status()).toBe(404);

    const teams = await (await page.request.get('/api/v1/teams')).json();
    const team = teams.teams.find((t: any) => t.id === seededTeam);
    expect(team, `seeded team ${seededTeam} is missing`).toBeTruthy();
    expect(team.on_call_configured).toBe(false);
  });

  /**
   * The one schedule no test mutates. Specs that need "a team that has one"
   * point at it; without it they latch onto another worker's fixture and fail
   * when that worker deletes it. Its absence used to surface as failures
   * somewhere else entirely.
   */
  test('the standing fixture exists and is active', async ({ page }) => {
    const config = await page.request.get('/api/v1/teams/e2e-standing/schedule/config');
    expect(config.status(),
      'e2e-standing is missing: did global.setup create it?').toBe(200);
    const body = await config.json();
    expect(body.deleted_at, 'the standing fixture must not be left deleted').toBeFalsy();
    expect(body.config.l1.groups.length).toBeGreaterThan(0);

    const teams = await (await page.request.get('/api/v1/teams')).json();
    const team = teams.teams.find((t: any) => t.id === 'e2e-standing');
    expect(team?.on_call_configured).toBe(true);
  });

  test('a new schedule starts at revision 1 and reads back as configured', async ({ page }) => {
    // On a team of its own, so this stays true however often it runs, and
    // through the shared fixture, so the team does not outlive the run: the
    // on-call overview lists every team, and one left behind per run is the
    // slow decay this fixture exists to prevent.
    const teamId = await fixtures.team('env', ['e2e-env-user']);

    const saved = await page.request.put(`/api/v1/teams/${teamId}/schedule/config`, {
      data: {
        expected_version: 0,
        timezone: 'UTC',
        l1: {
          enabled: true,
          rotation_type: 'daily',
          handoff_time: '09:00',
          handoff_day: null,
          groups: [{ id: crypto.randomUUID(), user_ids: ['e2e-env-user'] }],
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

    expect(saved.status(), await saved.text()).toBe(200);
    const body = await saved.json();
    expect(body.created).toBe(true);
    expect(body.version).toBe(1);

    // And the team reports itself configured - the read that used to answer
    // false for every revision-managed schedule.
    const teams = await (await page.request.get('/api/v1/teams')).json();
    expect(teams.teams.find((t: any) => t.id === teamId).on_call_configured).toBe(true);
  });
});
