import { test, expect } from '../../fixtures/auth.fixture';

test.describe('Policies CRUD', () => {
  test.beforeEach(async ({ policiesPage }) => {
    await policiesPage.goto();
    await policiesPage.waitForPoliciesLoad();
  });

  test('should navigate to Policies section', async ({ policiesPage, page }) => {
    await expect(page).toHaveURL(/#\/cfg\/policies/);
    await expect(policiesPage.policiesGrid).toBeVisible();
  });

  test('should show scope tabs (Team and Global)', async ({ policiesPage }) => {
    await expect(policiesPage.scopeTabTeam).toBeVisible();
    await expect(policiesPage.scopeTabGlobal).toBeVisible();
  });

  test('should switch between Team and Global scope tabs', async ({ policiesPage }) => {
    // Start with team scope
    await policiesPage.switchToTeamScope();
    await expect(policiesPage.scopeTabTeam).toHaveClass(/active/);

    // Switch to global scope
    await policiesPage.switchToGlobalScope();
    await expect(policiesPage.scopeTabGlobal).toHaveClass(/active/);

    // Switch back to team scope
    await policiesPage.switchToTeamScope();
    await expect(policiesPage.scopeTabTeam).toHaveClass(/active/);
  });

  test('should show create policy button', async ({ policiesPage }) => {
    await expect(policiesPage.createPolicyBtn).toBeVisible();
  });

  test('should open create policy modal', async ({ policiesPage }) => {
    await policiesPage.openCreatePolicyModal();
    await policiesPage.expectModalVisible();
  });

  test('should close create policy modal with cancel button', async ({ policiesPage }) => {
    await policiesPage.openCreatePolicyModal();
    await policiesPage.expectModalVisible();

    await policiesPage.closePolicyModal();
    await policiesPage.expectModalHidden();
  });

  test('should have required form fields in create policy modal', async ({ policiesPage }) => {
    await policiesPage.openCreatePolicyModal();

    await expect(policiesPage.policyNameInput).toBeVisible();
    await expect(policiesPage.policyTeamSelect).toBeVisible();
    await expect(policiesPage.addStepBtn).toBeVisible();
    await expect(policiesPage.savePolicyBtn).toBeVisible();
  });

  test('should have team options in team dropdown', async ({ policiesPage }) => {
    await policiesPage.openCreatePolicyModal();

    const options = policiesPage.policyTeamSelect.locator('option');
    const optionCount = await options.count();

    // Should have at least one team option
    expect(optionCount).toBeGreaterThanOrEqual(1);
  });

  test('should add step to policy', async ({ policiesPage }) => {
    await policiesPage.openCreatePolicyModal();

    // Initially might have no steps or one default step
    const initialStepCount = await policiesPage.getStepCount();

    await policiesPage.addStep();

    const newStepCount = await policiesPage.getStepCount();
    expect(newStepCount).toBe(initialStepCount + 1);
  });

  test('should remove step from policy', async ({ policiesPage }) => {
    await policiesPage.openCreatePolicyModal();

    // Add a step first
    await policiesPage.addStep();
    const stepCountAfterAdd = await policiesPage.getStepCount();

    if (stepCountAfterAdd > 0) {
      await policiesPage.removeStep(0);

      const stepCountAfterRemove = await policiesPage.getStepCount();
      expect(stepCountAfterRemove).toBe(stepCountAfterAdd - 1);
    }
  });

  test('should show validation error when policy name is empty', async ({ policiesPage }) => {
    await policiesPage.openCreatePolicyModal();

    // Don't fill in the name
    await policiesPage.addStep(); // Add at least one step

    await policiesPage.savePolicy();

    // Should show error toast
    await policiesPage.expectToastVisible('required');
  });

  test('should show validation error when no steps are added', async ({ policiesPage }) => {
    await policiesPage.openCreatePolicyModal();

    await policiesPage.policyNameInput.fill('Test Policy');

    // Get team option
    const firstOption = policiesPage.policyTeamSelect.locator('option:not([value=""])').first();
    const teamId = await firstOption.getAttribute('value');

    if (teamId) {
      await policiesPage.policyTeamSelect.selectOption(teamId);

      // Remove all steps if any exist
      const stepCount = await policiesPage.getStepCount();
      for (let i = stepCount - 1; i >= 0; i--) {
        await policiesPage.removeStep(i);
      }

      await policiesPage.savePolicy();

      // Should show error toast
      await policiesPage.expectToastVisible('step');
    }
  });

  test('should create policy with single step', async ({ policiesPage, page }) => {
    await policiesPage.openCreatePolicyModal();

    const policyName = `Test Policy ${Date.now()}`;

    // Get team option
    const firstOption = policiesPage.policyTeamSelect.locator('option:not([value=""])').first();
    const teamId = await firstOption.getAttribute('value');

    if (!teamId) {
      test.skip();
      return;
    }

    await policiesPage.policyNameInput.fill(policyName);
    await policiesPage.policyTeamSelect.selectOption(teamId);

    // Check if there are already steps, if not add one
    const stepRows = page.locator('.policy-step-row');
    let initialStepCount = await stepRows.count();

    if (initialStepCount === 0) {
      await policiesPage.addStep();
      await page.waitForTimeout(500);
      initialStepCount = await stepRows.count();
    }

    // Use the first step (which should be empty or we'll configure it)
    const stepRow = stepRows.first();
    await expect(stepRow).toBeVisible();

    // The target container has the user search input
    const targetContainer = stepRow.locator('.target-selector-container');
    await expect(targetContainer).toBeVisible();

    // Get the searchable user select within the step
    const searchInput = targetContainer.locator('.user-search-input').first();

    // Check if searchable select exists (user target type)
    if (await searchInput.count() === 0) {
      test.skip();
      return;
    }

    const datalistId = await searchInput.getAttribute('list');
    if (!datalistId) {
      test.skip();
      return;
    }

    // Wait for datalist options to be populated
    await page.waitForTimeout(300);
    const userOption = page.locator(`#${datalistId} option`).first();

    if (await userOption.count() === 0) {
      test.skip();
      return;
    }

    const userId = await userOption.getAttribute('data-user-id');
    const optionValue = await userOption.getAttribute('value');

    if (!userId) {
      test.skip();
      return;
    }

    // Set the display value
    await searchInput.fill(optionValue || '');

    // Wait a bit for the event handler to process
    await page.waitForTimeout(100);

    // Directly set the hidden input value - it's a sibling of the search input inside .searchable-select
    const hiddenInput = targetContainer.locator('.target-id-input');
    await hiddenInput.evaluate((el, id) => {
      (el as HTMLInputElement).value = id;
    }, userId);

    // Set up API response listener before clicking save
    const responsePromise = page.waitForResponse(
      (response) => response.url().includes('/api/v1/policies') && response.request().method() === 'POST'
    );

    await policiesPage.savePolicy();

    // Wait for API response
    const response = await responsePromise;
    expect([200, 201]).toContain(response.status());

    // Modal should close
    await policiesPage.expectModalHidden();

    // Should show success toast
    await policiesPage.expectToastVisible('created');
  });

  test('should add multiple steps to policy', async ({ policiesPage }) => {
    await policiesPage.openCreatePolicyModal();

    // Add multiple steps
    await policiesPage.addStep();
    await policiesPage.addStep();
    await policiesPage.addStep();

    const stepCount = await policiesPage.getStepCount();
    expect(stepCount).toBeGreaterThanOrEqual(3);
  });

  test('should open existing policy for editing', async ({ policiesPage }) => {
    // Try both team and global scopes to find existing policies
    let count = await policiesPage.policyCards.count();

    if (count === 0) {
      // Try global scope (seeded policies are global)
      await policiesPage.switchToGlobalScope();
      await policiesPage.waitForPoliciesLoad();
      count = await policiesPage.policyCards.count();
    }

    if (count > 0) {
      const firstCard = policiesPage.policyCards.first();
      const policyId = await firstCard.getAttribute('data-policy-id');

      if (policyId) {
        await policiesPage.openPolicyModal(policyId);
        await policiesPage.expectModalVisible();

        // Policy name should be pre-filled
        const nameValue = await policiesPage.policyNameInput.inputValue();
        expect(nameValue).toBeTruthy();
      }
    } else {
      test.skip();
    }
  });

  test('should edit existing policy', async ({ policiesPage, page }) => {
    // Try both team and global scopes to find existing policies
    let count = await policiesPage.policyCards.count();

    if (count === 0) {
      // Try global scope (seeded policies are global)
      await policiesPage.switchToGlobalScope();
      await policiesPage.waitForPoliciesLoad();
      count = await policiesPage.policyCards.count();
    }

    if (count > 0) {
      const firstCard = policiesPage.policyCards.first();
      const policyId = await firstCard.getAttribute('data-policy-id');

      if (policyId) {
        await policiesPage.openPolicyModal(policyId);
        await policiesPage.expectModalVisible();

        // Update policy name
        const newName = `Updated Policy ${Date.now()}`;
        await policiesPage.policyNameInput.fill(newName);

        // Set up API response listener
        const responsePromise = page.waitForResponse(
          (response) => response.url().includes(`/api/v1/policies/${policyId}`) && response.request().method() === 'PUT'
        );

        await policiesPage.savePolicy();

        // Wait for API response
        const response = await responsePromise;
        expect([200, 204]).toContain(response.status());

        // Modal should close
        await policiesPage.expectModalHidden();

        // Should show success toast
        await policiesPage.expectToastVisible('updated');
      }
    } else {
      test.skip();
    }
  });

  test('should delete policy with confirmation', async ({ policiesPage, page }) => {
    const policyName = `Delete Policy ${Date.now()}-${Math.random().toString(36).substring(2, 8)}`;

    await policiesPage.openCreatePolicyModal();
    await policiesPage.policyNameInput.fill(policyName);

    const teamOptions = policiesPage.policyTeamSelect.locator('option:not([value=""])');
    await expect.poll(async () => teamOptions.count(), { timeout: 10000 }).toBeGreaterThan(0);
    const teamId = await teamOptions.first().getAttribute('value');
    expect(teamId).toBeTruthy();
    if (!teamId) return;
    await policiesPage.policyTeamSelect.selectOption(teamId);

    const stepRows = page.locator('.policy-step-row');
    let initialStepCount = await stepRows.count();
    if (initialStepCount === 0) {
      await policiesPage.addStep();
      await page.waitForTimeout(300);
      initialStepCount = await stepRows.count();
    }

    const stepRow = stepRows.first();
    const targetContainer = stepRow.locator('.target-selector-container');
    const searchInput = targetContainer.locator('.user-search-input').first();
    await expect(searchInput).toBeVisible({ timeout: 10000 });

    const datalistId = await searchInput.getAttribute('list');
    expect(datalistId).toBeTruthy();
    if (!datalistId) return;

    await page.waitForTimeout(200);
    const userOption = page.locator(`#${datalistId} option`).first();
    await expect(userOption).toHaveCount(1, { timeout: 10000 });

    const userId = await userOption.getAttribute('data-user-id');
    const optionValue = await userOption.getAttribute('value');
    expect(userId).toBeTruthy();
    if (!userId) return;

    await searchInput.fill(optionValue || '');
    await page.waitForTimeout(100);

    const hiddenInput = targetContainer.locator('.target-id-input');
    await hiddenInput.evaluate((el, id) => {
      (el as HTMLInputElement).value = id;
    }, userId);

    await policiesPage.savePolicy();
    await policiesPage.expectModalHidden();
    await policiesPage.expectToastVisible('created');

    await policiesPage.switchToTeamScope();
    await policiesPage.waitForPoliciesLoad();

    const createdCard = page.locator('.policy-card').filter({ hasText: policyName }).first();
    await expect(createdCard).toBeVisible({ timeout: 10000 });

    const deleteBtn = createdCard.locator('.delete-policy-btn').first();
    await expect(deleteBtn).toBeVisible({ timeout: 10000 });

    page.once('dialog', dialog => dialog.accept());
    await deleteBtn.click();

    await policiesPage.expectToastVisible('deleted');
    await expect(page.locator('.policy-card').filter({ hasText: policyName })).toHaveCount(0, {
      timeout: 10000,
    });
  });
});

test.describe('Policies - Step Configuration', () => {
  test.beforeEach(async ({ policiesPage }) => {
    await policiesPage.goto();
    await policiesPage.waitForPoliciesLoad();
  });

  test('should show step type selector', async ({ policiesPage, page }) => {
    await policiesPage.openCreatePolicyModal();
    await policiesPage.addStep();

    const stepTypeSelect = page.locator('.step-type-select').first();
    await expect(stepTypeSelect).toBeVisible();
  });

  test('should show target type selector', async ({ policiesPage, page }) => {
    await policiesPage.openCreatePolicyModal();
    await policiesPage.addStep();

    const targetTypeSelect = page.locator('.target-type-select').first();
    await expect(targetTypeSelect).toBeVisible();
  });

  test('should show delay input', async ({ policiesPage, page }) => {
    await policiesPage.openCreatePolicyModal();
    await policiesPage.addStep();

    const delayInput = page.locator('.delay-input').first();
    await expect(delayInput).toBeVisible();
  });

  test('should configure step with user target', async ({ policiesPage, page }) => {
    await policiesPage.openCreatePolicyModal();
    await policiesPage.addStep();

    // Select user as target type
    const targetTypeSelect = page.locator('.target-type-select').first();
    await targetTypeSelect.selectOption('user');

    // User selector should appear
    const targetSelector = page.locator('.target-selector-container').first();
    await expect(targetSelector).toBeVisible();
  });

  test('should configure step delay', async ({ policiesPage, page }) => {
    await policiesPage.openCreatePolicyModal();
    await policiesPage.addStep();

    const delayInput = page.locator('.delay-input').first();
    await delayInput.fill('60');

    const value = await delayInput.inputValue();
    expect(value).toBe('60');
  });

  test('should configure step timeout', async ({ policiesPage, page }) => {
    await policiesPage.openCreatePolicyModal();
    await policiesPage.addStep();

    const timeoutInput = page.locator('.timeout-input').first();
    if (await timeoutInput.isVisible()) {
      await timeoutInput.fill('120');
      const value = await timeoutInput.inputValue();
      expect(value).toBe('120');
    }
  });
});

test.describe('Policies - Provider/Target Contracts (Epic 7)', () => {
  test.beforeEach(async ({ policiesPage }) => {
    await policiesPage.goto();
    await policiesPage.waitForPoliciesLoad();
  });

  // B1: changing the step type (provider:target_kind) must repopulate the target
  // type options — Slack Channel allows only "channel"; Slack DM allows user/schedule.
  test('step type switch updates target type options', async ({ policiesPage, page }) => {
    await policiesPage.openCreatePolicyModal();
    await policiesPage.addStep();

    const row = page.locator('.policy-step-row').first();
    const stepType = row.locator('.step-type-select');
    const targetType = row.locator('.target-type-select');

    await stepType.selectOption('slack:channel');
    await expect(targetType.locator('option[value="channel"]')).toHaveCount(1);
    await expect(targetType.locator('option[value="user"]')).toHaveCount(0);
    await expect(targetType.locator('option[value="schedule"]')).toHaveCount(0);

    await stepType.selectOption('slack:dm');
    await expect(targetType.locator('option[value="user"]')).toHaveCount(1);
    await expect(targetType.locator('option[value="schedule"]')).toHaveCount(1);
    await expect(targetType.locator('option[value="channel"]')).toHaveCount(0);
  });

  // B2: a persisted (provider, target_kind) pair that is no longer in the registry
  // must surface as an "(unknown)" option instead of silently switching step types.
  // We mock /api/v1/providers to exclude slack so the seeded slack step is "unknown".
  test('persisted unknown provider/kind renders as (unknown)', async ({ policiesPage, page }) => {
    await page.route('**/api/v1/providers', (route) =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          providers: [{ name: 'pagerduty', integration_type: 'pagerduty', supported_target_kinds: ['dm'] }],
        }),
      })
    );
    // Reload so the policy editor reads the mocked registry.
    await policiesPage.goto();
    await policiesPage.waitForPoliciesLoad();
    await policiesPage.switchToGlobalScope();

    // critical_policy is a seeded global policy with a slack DM step.
    await policiesPage.editPolicy('critical_policy');

    const stepType = page.locator('.policy-step-row').first().locator('.step-type-select');
    await expect(stepType.locator('option', { hasText: '(unknown)' })).toHaveCount(1);
  });

  // B3: duplicating a policy must re-send each step's provider + target_kind verbatim
  // (the P0 regression that step_type cannot silently return). We assert on the
  // outgoing create request body.
  test('duplicate policy request preserves provider and target_kind', async ({ policiesPage, page }) => {
    await policiesPage.switchToGlobalScope();

    const createReq = page.waitForRequest(
      (req) => req.url().includes('/api/v1/policies') && req.method() === 'POST'
    );

    await policiesPage.duplicatePolicy('critical_policy');
    await page.locator('#confirm-duplicate-btn').click();

    const body = (await createReq).postDataJSON();
    expect(Array.isArray(body.steps)).toBeTruthy();
    expect(body.steps.length).toBeGreaterThan(0);
    expect(body.steps[0].provider).toBe('slack');
    expect(body.steps[0].target_kind).toBe('dm');
  });
});

test.describe('Policies - Scope Filtering', () => {
  test.beforeEach(async ({ policiesPage }) => {
    await policiesPage.goto();
    await policiesPage.waitForPoliciesLoad();
  });

  test('should filter policies by team scope', async ({ policiesPage }) => {
    await policiesPage.switchToTeamScope();
    await expect(policiesPage.scopeTabTeam).toHaveClass(/active/);

    // Grid should be visible (may be empty or have policies)
    await expect(policiesPage.policiesGrid).toBeVisible();
  });

  test('should filter policies by global scope', async ({ policiesPage }) => {
    await policiesPage.switchToGlobalScope();
    await expect(policiesPage.scopeTabGlobal).toHaveClass(/active/);

    // Grid should be visible (may be empty or have policies)
    await expect(policiesPage.policiesGrid).toBeVisible();
  });
});
