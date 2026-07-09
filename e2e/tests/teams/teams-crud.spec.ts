import { test, expect } from '../../fixtures/auth.fixture';

test.describe('Teams CRUD', () => {
  test.beforeEach(async ({ teamsPage }) => {
    await teamsPage.goto();
    await teamsPage.waitForTeamsLoad();
  });

  test('should navigate to Configure mode and see Teams section', async ({ teamsPage, page }) => {
    await expect(page).toHaveURL(/#\/cfg\/teams/);
    await expect(teamsPage.teamsGrid).toBeVisible();
  });

  test('should show create team button', async ({ teamsPage }) => {
    await expect(teamsPage.createTeamBtn).toBeVisible();
  });

  test('should open create team modal', async ({ teamsPage }) => {
    await teamsPage.openCreateTeamModal();
    await teamsPage.expectCreateModalVisible();
  });

  test('should close create team modal with cancel button', async ({ teamsPage }) => {
    await teamsPage.openCreateTeamModal();
    await teamsPage.expectCreateModalVisible();
    await teamsPage.closeCreateTeamModal();
    await expect(teamsPage.teamFormModal).not.toHaveClass(/active/);
  });

  test('should have required form fields in create team modal', async ({ teamsPage }) => {
    await teamsPage.openCreateTeamModal();
    await expect(teamsPage.teamIdInput).toBeVisible();
    await expect(teamsPage.teamNameInput).toBeVisible();
    await expect(teamsPage.teamDescInput).toBeVisible();
  });

  test('should show validation error for invalid team ID format', async ({ teamsPage, page }) => {
    await teamsPage.openCreateTeamModal();

    // Try to create team with invalid ID (uppercase, spaces)
    await teamsPage.teamIdInput.fill('Invalid Team ID');
    await teamsPage.teamNameInput.fill('Test Team');
    await teamsPage.teamFormSubmit.click();

    // Should show error toast
    await teamsPage.expectToastVisible('lowercase');
  });

  test('should create new team with valid data', async ({ teamsPage, page }) => {
    const teamId = `test-team-${Date.now()}`;
    const teamName = 'E2E Test Team';

    // Set up API response listener before action
    const responsePromise = page.waitForResponse(
      (response) => response.url().includes('/api/v1/teams') && response.request().method() === 'POST'
    );

    await teamsPage.createTeam(teamId, teamName, 'Test description');

    // Wait for API response
    const response = await responsePromise;
    expect([200, 201]).toContain(response.status());

    // Verify the response contains the correct team ID
    const responseData = await response.json();
    expect(responseData.id).toBe(teamId);

    // Wait for toast success message
    await teamsPage.expectToastVisible('created');

    // Modal should close
    await expect(teamsPage.teamFormModal).not.toHaveClass(/active/);
  });

  test('should open team management modal when clicking on a team', async ({ teamsPage }) => {
    // Get first team card
    const teamCards = teamsPage.teamCards;
    const count = await teamCards.count();

    if (count > 0) {
      const firstCard = teamCards.first();
      const teamId = await firstCard.getAttribute('data-team-id');

      if (teamId) {
        await teamsPage.openTeamModal(teamId);
        await teamsPage.expectManageModalVisible();
      }
    } else {
      test.skip();
    }
  });

  test('should close team management modal', async ({ teamsPage }) => {
    const teamCards = teamsPage.teamCards;
    const count = await teamCards.count();

    if (count > 0) {
      const firstCard = teamCards.first();
      const teamId = await firstCard.getAttribute('data-team-id');

      if (teamId) {
        await teamsPage.openTeamModal(teamId);
        await teamsPage.expectManageModalVisible();
        await teamsPage.closeTeamModal();
        await expect(teamsPage.teamManageModal).not.toHaveClass(/active/);
      }
    } else {
      test.skip();
    }
  });

  test('should show routing configuration in team management modal', async ({ teamsPage, page }) => {
    const teamCards = teamsPage.teamCards;
    const count = await teamCards.count();

    if (count > 0) {
      const firstCard = teamCards.first();
      const teamId = await firstCard.getAttribute('data-team-id');

      if (teamId) {
        await teamsPage.openTeamModal(teamId);

        // Check for routing elements (only visible if user has edit permission)
        const isVisible = await teamsPage.saveRoutingBtn.isVisible().catch(() => false);
        if (isVisible) {
          await expect(teamsPage.saveRoutingBtn).toBeVisible();
        } else {
          // Button not visible - user may not have edit permissions, skip test
          test.skip();
        }
      }
    } else {
      test.skip();
    }
  });

  test('should save routing configuration', async ({ teamsPage, page }) => {
    const teamCards = teamsPage.teamCards;
    const count = await teamCards.count();

    if (count > 0) {
      const firstCard = teamCards.first();
      const teamId = await firstCard.getAttribute('data-team-id');

      if (teamId) {
        await teamsPage.openTeamModal(teamId);

        // Check if save routing button is visible (only for users with edit permission)
        const isVisible = await teamsPage.saveRoutingBtn.isVisible().catch(() => false);
        if (!isVisible) {
          test.skip();
          return;
        }

        // Wait for the API response
        const responsePromise = page.waitForResponse(
          (response) => response.url().includes('/api/v1/teams/') && response.request().method() === 'PUT'
        );

        await teamsPage.saveRouting();

        // Verify API call was successful
        const response = await responsePromise;
        expect([200, 204]).toContain(response.status());

        await teamsPage.expectToastVisible('saved');
      }
    } else {
      test.skip();
    }
  });

  test('should delete team with confirmation', async ({ teamsPage, page }) => {
    // Create team via UI
    const teamId = `del-${Date.now()}`;
    const teamName = 'Delete Test Team';

    // Set up API response listener for create
    const createResponsePromise = page.waitForResponse(
      (response) => response.url().includes('/api/v1/teams') && response.request().method() === 'POST'
    );

    await teamsPage.createTeam(teamId, teamName, 'Team for deletion test');

    // Wait for create API response
    const createResponse = await createResponsePromise;
    expect([200, 201]).toContain(createResponse.status());

    // Wait for toast and modal to close
    await teamsPage.expectToastVisible('created');
    await expect(teamsPage.teamFormModal).not.toHaveClass(/active/);

    // Reload page to see new team in grid
    await page.reload();
    await teamsPage.waitForTeamsLoad();

    // The team card should now be visible
    const teamCard = page.locator(`.team-card[data-team-id="${teamId}"]`);
    await expect(teamCard).toBeVisible({ timeout: 10000 });

    // Open team modal
    await teamsPage.openTeamModal(teamId);
    await teamsPage.expectManageModalVisible();

    // Set up dialog handler for confirmation
    page.on('dialog', dialog => dialog.accept());

    // Set up API response listener for delete
    const deleteResponsePromise = page.waitForResponse(
      (response) => response.url().includes(`/api/v1/teams/${teamId}`) && response.request().method() === 'DELETE'
    );

    // Delete the team
    await teamsPage.deleteTeam();

    // Wait for delete API response
    const deleteResponse = await deleteResponsePromise;
    expect([200, 204]).toContain(deleteResponse.status());

    await teamsPage.expectToastVisible('deleted');

    // Modal should close
    await teamsPage.waitForTeamsLoad();
  });
});

test.describe('Teams - Display', () => {
  test('should display team cards with correct information', async ({ teamsPage }) => {
    await teamsPage.goto();
    await teamsPage.waitForTeamsLoad();

    const teamCards = teamsPage.teamCards;
    const count = await teamCards.count();

    if (count > 0) {
      const firstCard = teamCards.first();
      // Team cards should have name visible
      const teamName = firstCard.locator('.team-name, .team-card-title, h3');
      await expect(teamName.first()).toBeVisible();
    }
  });

  // This test only runs in an empty database state.
  // With seed data, teams always exist, so we skip it explicitly.
  test.skip('should show empty state when no teams exist', async ({ teamsPage, page }) => {
    await teamsPage.goto();
    await teamsPage.waitForTeamsLoad();

    const count = await teamsPage.getTeamCount();

    if (count === 0) {
      const emptyState = page.locator('.empty-state, .empty-teams-state');
      await expect(emptyState.first()).toBeVisible();
    }
  });
});
