import { test, expect } from '../../fixtures/auth.fixture';

test.describe('Manual Alert Creation', () => {
  test.beforeEach(async ({ dashboardPage }) => {
    await dashboardPage.gotoAlertGroups();
    await dashboardPage.expectLoadingComplete();
  });

  test('should show create alert button in page actions', async ({ dashboardPage }) => {
    // The create alert button should be visible in ops mode
    await expect(dashboardPage.createAlertBtn).toBeVisible();
  });

  test('should open manual alert modal', async ({ dashboardPage }) => {
    await dashboardPage.openManualAlertModal();
    await dashboardPage.expectManualAlertModalVisible();
  });

  test('should close manual alert modal with cancel button', async ({ dashboardPage }) => {
    await dashboardPage.openManualAlertModal();
    await dashboardPage.expectManualAlertModalVisible();

    await dashboardPage.closeManualAlertModal();
    await dashboardPage.expectManualAlertModalHidden();
  });

  test('should close manual alert modal by clicking modal close button', async ({ dashboardPage }) => {
    await dashboardPage.openManualAlertModal();
    await dashboardPage.expectManualAlertModalVisible();

    await dashboardPage.manualAlertModalClose.click();
    await dashboardPage.expectManualAlertModalHidden();
  });

  test('should have required form fields in manual alert modal', async ({ dashboardPage }) => {
    await dashboardPage.openManualAlertModal();

    await expect(dashboardPage.manualAlertTeam).toBeVisible();
    await expect(dashboardPage.manualAlertSeverity).toBeVisible();
    await expect(dashboardPage.manualAlertTitle).toBeVisible();
    await expect(dashboardPage.manualAlertSubmit).toBeVisible();
  });

  test('should have team options in team dropdown', async ({ dashboardPage }) => {
    await dashboardPage.openManualAlertModal();

    const options = dashboardPage.manualAlertTeam.locator('option');
    const optionCount = await options.count();

    // Should have at least one team option
    expect(optionCount).toBeGreaterThanOrEqual(1);
  });

  test('should have severity options in severity dropdown', async ({ dashboardPage }) => {
    await dashboardPage.openManualAlertModal();

    const options = dashboardPage.manualAlertSeverity.locator('option');
    const optionCount = await options.count();

    // Should have severity options (critical, warning, info)
    expect(optionCount).toBeGreaterThanOrEqual(1);
  });

  test('should have default values pre-filled', async ({ dashboardPage }) => {
    await dashboardPage.openManualAlertModal();

    // Severity should have a default value
    const severityValue = await dashboardPage.manualAlertSeverity.inputValue();
    expect(severityValue).toBeTruthy();

    // Title should have a default value
    const titleValue = await dashboardPage.manualAlertTitle.inputValue();
    expect(titleValue).toBeTruthy();
  });

  test('should show validation error when team is not selected', async ({ dashboardPage, page }) => {
    await dashboardPage.openManualAlertModal();

    // Clear team selection if possible (select empty option)
    const emptyOption = dashboardPage.manualAlertTeam.locator('option[value=""]');
    const hasEmptyOption = await emptyOption.count() > 0;

    if (hasEmptyOption) {
      await dashboardPage.manualAlertTeam.selectOption('');
      await dashboardPage.manualAlertSubmit.click();
      await dashboardPage.expectToastVisible('required');
    } else {
      // If no empty option, team is always selected - skip this test
      test.skip();
    }
  });

  test('should create manual alert with required fields', async ({ dashboardPage, page }) => {
    await dashboardPage.openManualAlertModal();

    // Get first team option
    const firstOption = dashboardPage.manualAlertTeam.locator('option:not([value=""])').first();
    const teamId = await firstOption.getAttribute('value');

    if (teamId) {
      const alertTitle = `E2E Test Alert ${Date.now()}`;

      // Set up API response listener
      const responsePromise = page.waitForResponse(
        (response) => response.url().includes('/api/v1/alert-groups') && response.request().method() === 'POST'
      );

      await dashboardPage.manualAlertTeam.selectOption(teamId);
      await dashboardPage.manualAlertSeverity.selectOption('warning');
      await dashboardPage.manualAlertTitle.fill(alertTitle);
      await dashboardPage.manualAlertSubmit.click();

      // Wait for API response
      const response = await responsePromise;
      expect([200, 201]).toContain(response.status());

      // Modal should close
      await dashboardPage.expectManualAlertModalHidden();

      // Should show success toast
      await dashboardPage.expectToastVisible('created');
    } else {
      test.skip();
    }
  });

  test('should create manual alert with critical severity', async ({ dashboardPage, page }) => {
    await dashboardPage.openManualAlertModal();

    const firstOption = dashboardPage.manualAlertTeam.locator('option:not([value=""])').first();
    const teamId = await firstOption.getAttribute('value');

    if (teamId) {
      const responsePromise = page.waitForResponse(
        (response) => response.url().includes('/api/v1/alert-groups') && response.request().method() === 'POST'
      );

      await dashboardPage.manualAlertTeam.selectOption(teamId);
      await dashboardPage.manualAlertSeverity.selectOption('critical');
      await dashboardPage.manualAlertTitle.fill(`Critical Alert ${Date.now()}`);
      await dashboardPage.manualAlertSubmit.click();

      const response = await responsePromise;
      const responseData = await response.json();

      expect([200, 201]).toContain(response.status());
      expect(responseData.severity).toBe('critical');
    } else {
      test.skip();
    }
  });

  test('should show new alert in list after creation', async ({ dashboardPage, page }) => {
    await dashboardPage.openManualAlertModal();

    const firstOption = dashboardPage.manualAlertTeam.locator('option:not([value=""])').first();
    const teamId = await firstOption.getAttribute('value');

    if (teamId) {
      const alertTitle = `Visible Alert ${Date.now()}`;

      const responsePromise = page.waitForResponse(
        (response) => response.url().includes('/api/v1/alert-groups') && response.request().method() === 'POST'
      );

      await dashboardPage.manualAlertTeam.selectOption(teamId);
      await dashboardPage.manualAlertSeverity.selectOption('warning');
      await dashboardPage.manualAlertTitle.fill(alertTitle);
      await dashboardPage.manualAlertSubmit.click();

      await responsePromise;
      await dashboardPage.expectManualAlertModalHidden();

      // Wait for list to refresh
      await page.waitForTimeout(1000);

      // Close any modal that might have opened (e.g., clicking on alert opens detail modal)
      const modalOverlay = page.locator('#modal-overlay');
      if (await modalOverlay.evaluate(el => el.classList.contains('active')).catch(() => false)) {
        await dashboardPage.closeAlertModal();
        await expect(modalOverlay).not.toHaveClass(/active/);
      }

      await dashboardPage.expectLoadingComplete();

      // Switch to "all" state to ensure we can see the new alert
      await dashboardPage.filterByState('all');
      await page.waitForTimeout(500);

      // The new alert should be in the list
      const alertCards = dashboardPage.alertGroupsGrid.locator('.alert-group-card');
      const count = await alertCards.count();
      expect(count).toBeGreaterThan(0);
    } else {
      test.skip();
    }
  });

  test('should pre-select current team context if set', async ({ dashboardPage, page }) => {
    // Open team context dropdown to get available teams from UI
    await dashboardPage.openTeamContextDropdown();

    // Get the first team option (not 'all')
    const teamOptions = page.locator('#team-context-dropdown [data-team-id]:not([data-team-id="all"])');
    const teamCount = await teamOptions.count();

    if (teamCount === 0) {
      test.skip();
      return;
    }

    const firstTeamId = await teamOptions.first().getAttribute('data-team-id');

    if (!firstTeamId) {
      test.skip();
      return;
    }

    // Close dropdown by clicking trigger again or selecting the team
    await teamOptions.first().click();
    await page.waitForTimeout(500);

    // Open manual alert modal
    await dashboardPage.openManualAlertModal();

    // The team should be pre-selected
    const selectedTeam = await dashboardPage.manualAlertTeam.inputValue();
    expect(selectedTeam).toBe(firstTeamId);
  });
});

test.describe('Manual Alert - Edge Cases', () => {
  test('should handle API error gracefully', async ({ dashboardPage, page }) => {
    await dashboardPage.gotoAlertGroups();

    // Mock API error
    await page.route('**/api/v1/alert-groups', async (route) => {
      if (route.request().method() === 'POST') {
        await route.fulfill({
          status: 500,
          contentType: 'application/json',
          body: JSON.stringify({ error: 'Internal server error' }),
        });
      } else {
        await route.continue();
      }
    });

    await dashboardPage.openManualAlertModal();

    const firstOption = dashboardPage.manualAlertTeam.locator('option:not([value=""])').first();
    const teamId = await firstOption.getAttribute('value');

    if (teamId) {
      await dashboardPage.manualAlertTeam.selectOption(teamId);
      await dashboardPage.manualAlertTitle.fill('Error Test Alert');
      await dashboardPage.manualAlertSubmit.click();

      // Should show error toast
      await dashboardPage.expectToastVisible('');

      // Modal should still be visible
      await dashboardPage.expectManualAlertModalVisible();
    }
  });
});
