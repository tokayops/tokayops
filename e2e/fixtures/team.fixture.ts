import { Page, expect } from '@playwright/test';

/**
 * Teams created for a test, and their removal.
 *
 * Left behind, they accumulate: the on-call overview draws a row per team and
 * asks each one who is on duty, so every fixture that outlives its test makes
 * every later test slower. That compounds across a run until the slower
 * browser starts timing out, which reads as flakiness and is really litter.
 *
 * This lives in one place rather than as a module-level array in whichever
 * spec happened to need it: a second spec doing the same thing would keep its
 * own list, and a fixture created outside that list is exactly the one nobody
 * cleans up.
 */
export class TeamFixtures {
  private readonly created: string[] = [];

  constructor(private readonly page: Page) {}

  /**
   * A team with the given members, all of whom exist and are in it.
   *
   * @param prefix - names the test in the team id, so a leak is traceable
   * @param memberIds - created if missing; the ids are shared across tests on
   *        purpose, since users are not what accumulates
   */
  async team(prefix: string, memberIds: string[]): Promise<string> {
    const teamId = `e2e-${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    this.created.push(teamId);

    const created = await this.page.request.post('/api/v1/teams', {
      data: { id: teamId, name: `E2E ${prefix}` },
    });
    expect([200, 201]).toContain(created.status());

    for (const id of memberIds) {
      await this.page.request.post('/api/v1/users', {
        data: { id, email: `${id}@test.com`, name: id.replace('e2e-', 'E2E ') },
      });
      const added = await this.page.request.post(`/api/v1/teams/${teamId}/members`, {
        data: { user_id: id, role: 'team_member' },
      });
      expect([200, 201]).toContain(added.status());
    }

    return teamId;
  }

  /**
   * Remove the teams this test created, as far as the server allows.
   *
   * See `deleteTeam`: a team that has ever had a schedule is retained, not
   * removed, and that is not something this fixture can change.
   */
  async cleanup(): Promise<void> {
    const teams = this.created.splice(0);
    const failures: string[] = [];

    for (const id of teams) {
      const outcome = await deleteTeam(this.page, id);
      if (outcome.result === 'failed') failures.push(`${id}: ${outcome.detail}`);
    }

    expect(failures, `teams left behind unexpectedly: ${failures.join('; ')}`).toEqual([]);
  }
}

export type DeleteOutcome =
  | { result: 'deleted' }
  | { result: 'retained'; reason: 'schedule-history' }
  | { result: 'failed'; detail: string };

/**
 * Delete a team, and say honestly what happened.
 *
 * Every team deletion in the suite goes through here. The status has to be
 * inspected: a Playwright request resolves on any status, so ignoring the
 * response - which each spec used to do in its own way - counts an HTTP 500 as
 * a successful cleanup.
 *
 * `retained` is a real outcome, not a soft failure. A team that has ever had a
 * schedule cannot be deleted at all: `teams -> schedules` cascades,
 * `schedules -> schedule_revisions` restricts, and the restriction wins - by
 * design, since history must not be cascaded away. Soft-deleting the schedule
 * first does not help; the revisions remain. Recorded as TD10.
 *
 * Sprint 6 will turn the driver error into an explicit 409, which makes the
 * answer legible but leaves the team exactly where it is. So these teams live
 * until the volume is destroyed, which `make e2e-test` does on every run. What
 * this fixture buys is that everything else really is removed, and that a new
 * kind of failure is not mistaken for the known one.
 */
export async function deleteTeam(page: Page, teamId: string): Promise<DeleteOutcome> {
  const response = await page.request.delete(`/api/v1/teams/${teamId}`);
  if ([200, 204, 404].includes(response.status())) return { result: 'deleted' };

  const body = await response.text();

  // Two shapes, each recognised on its own terms.
  //
  // Today the refusal escapes as a driver error, so there is nothing to read
  // but the constraint name - and it is only accepted with the 500 that
  // carries it. Once Sprint 6 gives the refusal a machine code, it arrives as
  // a 409 and is recognised by that code, not by finding the string somewhere
  // in the response: a code exists precisely so that nobody parses text for
  // it. Matching loosely would also accept a 409 that merely mentioned the
  // name while meaning something else.
  //
  // Any other 500, and any other 409, is a failure.
  if (response.status() === 500 && body.includes('schedule_revisions_schedule_id_fkey')) {
    return { result: 'retained', reason: 'schedule-history' };
  }
  if (response.status() === 409) {
    let code: unknown;
    try {
      code = JSON.parse(body)?.code;
    } catch {
      code = undefined;
    }
    if (code === 'team_has_schedule_history') {
      return { result: 'retained', reason: 'schedule-history' };
    }
  }

  return { result: 'failed', detail: `${response.status()} ${body.slice(0, 120)}` };
}
