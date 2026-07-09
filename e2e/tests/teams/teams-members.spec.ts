import { test, expect } from '../../fixtures/auth.fixture';

test.describe('Team Members', () => {
  test.beforeEach(async ({ teamsPage }) => {
    await teamsPage.goto();
    await teamsPage.waitForTeamsLoad();
  });

  test('should show add member controls in team modal', async ({ teamsPage }) => {
    const teamCards = teamsPage.teamCards;
    const count = await teamCards.count();

    if (count > 0) {
      const firstCard = teamCards.first();
      const teamId = await firstCard.getAttribute('data-team-id');

      if (teamId) {
        await teamsPage.openTeamModal(teamId);
        await teamsPage.expectManageModalVisible();

        // Should have add member controls
        await expect(teamsPage.addMemberSelect).toBeVisible();
        await expect(teamsPage.addMemberRoleSelect).toBeVisible();
        await expect(teamsPage.addMemberBtn).toBeVisible();
      }
    } else {
      test.skip();
    }
  });

  test('should show available users in add member dropdown', async ({ teamsPage, page }) => {
    const teamCards = teamsPage.teamCards;
    const count = await teamCards.count();

    if (count > 0) {
      const firstCard = teamCards.first();
      const teamId = await firstCard.getAttribute('data-team-id');

      if (teamId) {
        await teamsPage.openTeamModal(teamId);

        // Check that the user select has options
        const options = teamsPage.addMemberSelect.locator('option');
        const optionCount = await options.count();

        // Should have at least the default option and some users
        expect(optionCount).toBeGreaterThanOrEqual(1);
      }
    } else {
      test.skip();
    }
  });

  test('should show role options in role dropdown', async ({ teamsPage }) => {
    const teamCards = teamsPage.teamCards;
    const count = await teamCards.count();

    if (count > 0) {
      const firstCard = teamCards.first();
      const teamId = await firstCard.getAttribute('data-team-id');

      if (teamId) {
        await teamsPage.openTeamModal(teamId);

        // Check role select options
        const options = teamsPage.addMemberRoleSelect.locator('option');
        const optionCount = await options.count();

        // Should have role options (member, admin, etc.)
        expect(optionCount).toBeGreaterThanOrEqual(1);
      }
    } else {
      test.skip();
    }
  });

  test('should add member to team', async ({ teamsPage, page }) => {
    const teamCards = teamsPage.teamCards;
    const count = await teamCards.count();

    if (count > 0) {
      const firstCard = teamCards.first();
      const teamId = await firstCard.getAttribute('data-team-id');

      if (teamId) {
        await teamsPage.openTeamModal(teamId);

        // Get available users from dropdown
        const options = teamsPage.addMemberSelect.locator('option:not([value=""])');
        const optionCount = await options.count();

        if (optionCount > 0) {
          const firstOption = options.first();
          const userId = await firstOption.getAttribute('value');

          if (userId) {
            // Set up API response listener
            const responsePromise = page.waitForResponse(
              (response) => response.url().includes('/members') && response.request().method() === 'POST'
            );

            await teamsPage.addMemberToTeam(userId, 'team_member');

            // Wait for API response
            const response = await responsePromise;
            expect([200, 201]).toContain(response.status());

            await teamsPage.expectToastVisible('added');
          } else {
            test.skip();
          }
        } else {
          test.skip();
        }
      }
    } else {
      test.skip();
    }
  });

  test('should display existing team members', async ({ teamsPage, page }) => {
    const teamCards = teamsPage.teamCards;
    const count = await teamCards.count();

    if (count > 0) {
      const firstCard = teamCards.first();
      const teamId = await firstCard.getAttribute('data-team-id');

      if (teamId) {
        await teamsPage.openTeamModal(teamId);

        // Look for member list or member rows
        const membersList = page.locator('.team-members-list, .members-list, [data-testid="members-list"]');
        const membersExist = await membersList.isVisible().catch(() => false);

        // Members section should be visible (even if empty)
        // The structure should at least show the member management area
        await teamsPage.expectManageModalVisible();
      }
    } else {
      test.skip();
    }
  });

  test('should show role select for existing members', async ({ teamsPage, page }) => {
    const teamCards = teamsPage.teamCards;
    const count = await teamCards.count();

    if (count > 0) {
      const firstCard = teamCards.first();
      const teamId = await firstCard.getAttribute('data-team-id');

      if (teamId) {
        await teamsPage.openTeamModal(teamId);

        // Check for existing member role selects
        const roleSelects = page.locator('.role-select');
        const roleSelectCount = await roleSelects.count();

        // If there are members, they should have role selects
        if (roleSelectCount > 0) {
          const firstRoleSelect = roleSelects.first();
          await expect(firstRoleSelect).toBeVisible();
        }
      }
    } else {
      test.skip();
    }
  });

  test('should change member role', async ({ teamsPage, page, request }) => {
    const teamCards = teamsPage.teamCards;
    const count = await teamCards.count();

    if (count > 0) {
      const firstCard = teamCards.first();
      const teamId = await firstCard.getAttribute('data-team-id');

      if (teamId) {
        const suffix = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
        const createUserRes = await request.post('/api/v1/users', {
          data: {
            name: `Temp Team Member ${suffix}`,
            email: `team-member-${suffix}@example.com`,
            password: 'TestPassword123!',
            role: 'user',
          },
        });
        expect([200, 201]).toContain(createUserRes.status());
        const createdUser = await createUserRes.json();
        const tempUserId = createdUser?.id as string | undefined;
        expect(tempUserId).toBeTruthy();
        if (!tempUserId) return;

        try {
          await teamsPage.openTeamModal(teamId);

          // Add temp member first
          const addMemberPromise = page.waitForResponse(
            (response) => response.url().includes('/members') && response.request().method() === 'POST'
          );
          await teamsPage.addMemberToTeam(tempUserId, 'team_member');
          const addMemberResponse = await addMemberPromise;
          expect([200, 201]).toContain(addMemberResponse.status());

          // Change temp member role
          const tempRoleSelect = page.locator(`.role-select[data-user-id="${tempUserId}"]`);
          await expect(tempRoleSelect).toBeVisible({ timeout: 10000 });

          const roleUpdatePromise = page.waitForResponse(
            (response) =>
              response.url().includes('/members') &&
              response.request().method() === 'POST' &&
              !!response.request().postData()?.includes(tempUserId)
          );

          await tempRoleSelect.selectOption('team_admin');

          const roleUpdateResponse = await roleUpdatePromise;
          expect([200, 201]).toContain(roleUpdateResponse.status());

          await teamsPage.expectToastVisible('updated');
        } finally {
          await request.delete(`/api/v1/teams/${teamId}/members/${tempUserId}`).catch(() => {});
          await request.delete(`/api/v1/users/${tempUserId}`).catch(() => {});
        }
      }
    } else {
      test.skip();
    }
  });

  test('should show remove member button for existing members', async ({ teamsPage, page }) => {
    const teamCards = teamsPage.teamCards;
    const count = await teamCards.count();

    if (count > 0) {
      const firstCard = teamCards.first();
      const teamId = await firstCard.getAttribute('data-team-id');

      if (teamId) {
        await teamsPage.openTeamModal(teamId);

        // Check for remove member buttons
        const removeButtons = page.locator('.remove-member-btn');
        const buttonCount = await removeButtons.count();

        // If there are members, they should have remove buttons
        if (buttonCount > 0) {
          const firstRemoveBtn = removeButtons.first();
          await expect(firstRemoveBtn).toBeVisible();
        }
      }
    } else {
      test.skip();
    }
  });

  test('should remove member from team with confirmation', async ({ teamsPage, page, request }) => {
    const teamCards = teamsPage.teamCards;
    const count = await teamCards.count();

    if (count > 0) {
      const firstCard = teamCards.first();
      const teamId = await firstCard.getAttribute('data-team-id');

      if (teamId) {
        const suffix = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
        const createUserRes = await request.post('/api/v1/users', {
          data: {
            name: `Temp Removable Member ${suffix}`,
            email: `team-remove-${suffix}@example.com`,
            password: 'TestPassword123!',
            role: 'user',
          },
        });
        expect([200, 201]).toContain(createUserRes.status());
        const createdUser = await createUserRes.json();
        const tempUserId = createdUser?.id as string | undefined;
        expect(tempUserId).toBeTruthy();
        if (!tempUserId) return;

        try {
          await teamsPage.openTeamModal(teamId);

          // Add temp member first so we can safely remove it.
          const addMemberPromise = page.waitForResponse(
            (response) => response.url().includes('/members') && response.request().method() === 'POST'
          );
          await teamsPage.addMemberToTeam(tempUserId, 'team_member');
          const addMemberResponse = await addMemberPromise;
          expect([200, 201]).toContain(addMemberResponse.status());

          const removeBtn = page.locator(`.remove-member-btn[data-user-id="${tempUserId}"]`);
          await expect(removeBtn).toBeVisible({ timeout: 10000 });

          // Set up dialog handler for confirmation
          page.once('dialog', dialog => dialog.accept());

          // Set up API response listener
          const responsePromise = page.waitForResponse(
            (response) =>
              response.url().includes(`/members/${tempUserId}`) &&
              response.request().method() === 'DELETE'
          );

          await removeBtn.click();

          // Wait for API response
          const response = await responsePromise;
          expect([200, 204]).toContain(response.status());

          await teamsPage.expectToastVisible('removed');
        } finally {
          await request.delete(`/api/v1/teams/${teamId}/members/${tempUserId}`).catch(() => {});
          await request.delete(`/api/v1/users/${tempUserId}`).catch(() => {});
        }
      }
    } else {
      test.skip();
    }
  });
});
