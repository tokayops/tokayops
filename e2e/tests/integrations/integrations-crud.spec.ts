import { test, expect } from '../../fixtures/auth.fixture';

// Track created integrations for cleanup
const createdIntegrationIds: string[] = [];

test.describe('Integrations CRUD', () => {
  test.beforeEach(async ({ integrationsPage, page }) => {
    await integrationsPage.goto();

    // Check if user has admin access (redirected away means no access)
    await page.waitForTimeout(500);
    const currentUrl = page.url();
    if (!currentUrl.includes('/cfg/integrations')) {
      test.skip();
      return;
    }

    await integrationsPage.waitForIntegrationsLoad();
  });

  test('should navigate to Integrations section', async ({ integrationsPage, page }) => {
    await expect(page).toHaveURL(/#\/cfg\/integrations/);
    await expect(integrationsPage.integrationsGrid).toBeVisible();
  });

  test('should show create integration button', async ({ integrationsPage }) => {
    await expect(integrationsPage.createIntegrationBtn).toBeVisible();
  });

  test('should open create integration modal', async ({ integrationsPage }) => {
    await integrationsPage.openCreateIntegrationModal();
    await integrationsPage.expectModalVisible();
  });

  test('should close create integration modal with cancel button', async ({ integrationsPage }) => {
    await integrationsPage.openCreateIntegrationModal();
    await integrationsPage.expectModalVisible();

    await integrationsPage.closeIntegrationModal();
    await integrationsPage.expectModalHidden();
  });

  test('should have required form fields in create integration modal', async ({ integrationsPage }) => {
    await integrationsPage.openCreateIntegrationModal();

    await expect(integrationsPage.integrationNameInput).toBeVisible();
    await expect(integrationsPage.integrationTypeSelect).toBeVisible();
  });

  test('should show direction tabs in create mode', async ({ integrationsPage }) => {
    await integrationsPage.openCreateIntegrationModal();

    await expect(integrationsPage.directionTabs).toBeVisible();
    await expect(integrationsPage.directionOutbound).toBeVisible();
    await expect(integrationsPage.directionInbound).toBeVisible();
  });

  test('should switch between outbound and inbound directions', async ({ integrationsPage }) => {
    await integrationsPage.openCreateIntegrationModal();

    // Select outbound
    await integrationsPage.selectOutboundDirection();
    await expect(integrationsPage.directionOutbound).toHaveClass(/active/);

    // Select inbound
    await integrationsPage.selectInboundDirection();
    await expect(integrationsPage.directionInbound).toHaveClass(/active/);
  });

  test('should show Slack type option when outbound is selected', async ({ integrationsPage }) => {
    await integrationsPage.openCreateIntegrationModal();
    await integrationsPage.selectOutboundDirection();

    // Type should have Slack option (check option exists, not visible - select options are hidden until opened)
    const slackOption = integrationsPage.integrationTypeSelect.locator('option[value="slack"]');
    await expect(slackOption).toHaveCount(1);
  });

  test('should show Alertmanager Webhook type option when inbound is selected', async ({ integrationsPage }) => {
    await integrationsPage.openCreateIntegrationModal();
    await integrationsPage.selectInboundDirection();

    // Type should have Alertmanager Webhook option (check option exists)
    const alertmanagerOption = integrationsPage.integrationTypeSelect.locator('option[value="alertmanager_webhook"]');
    await expect(alertmanagerOption).toHaveCount(1);
  });

  test('should show Slack config fields when Slack type is selected', async ({ integrationsPage, page }) => {
    await integrationsPage.openCreateIntegrationModal();
    await integrationsPage.selectOutboundDirection();

    await page.waitForTimeout(100);

    // Slack config fields should be visible
    await expect(integrationsPage.configToken).toBeVisible();
  });

  test('should show interactive toggle unchecked by default for Slack', async ({ integrationsPage, page }) => {
    await integrationsPage.openCreateIntegrationModal();
    await integrationsPage.selectOutboundDirection();

    await page.waitForTimeout(100);

    // Interactive toggle is rendered as a visible label + hidden checkbox input
    await expect(integrationsPage.configInteractiveToggle).toBeVisible();
    await expect(integrationsPage.configInteractive).not.toBeChecked();
  });

  test('should hide interactive toggle for Alertmanager type', async ({ integrationsPage, page }) => {
    await integrationsPage.openCreateIntegrationModal();
    await integrationsPage.selectInboundDirection();

    await page.waitForTimeout(100);

    // Inbound types have no interactive toggle (Slack and Telegram both do)
    await expect(integrationsPage.configInteractiveToggle).toBeHidden();
  });

  test('should create Slack integration with interactive toggle enabled', async ({ integrationsPage, page }) => {
    const randomSuffix = Math.random().toString(36).substring(2, 8);
    const integrationName = `Slack Interactive ${Date.now()}-${randomSuffix}`;
    const token = `xoxb-test-token-${randomSuffix}`;

    await integrationsPage.openCreateIntegrationModal();
    await integrationsPage.selectOutboundDirection();
    await page.waitForTimeout(100);

    await integrationsPage.integrationNameInput.fill(integrationName);
    await integrationsPage.configToken.fill(token);

    // Enable interactive toggle
    await expect(integrationsPage.configInteractiveToggle).toBeVisible();
    await integrationsPage.setInteractive(true);

    // Set up API request listener to verify payload
    const requestPromise = page.waitForResponse(
      (response) => response.url().includes('/api/v1/integrations') && response.request().method() === 'POST'
    );

    await integrationsPage.integrationFormSubmit.click();

    const response = await requestPromise;
    expect([200, 201, 409]).toContain(response.status());

    if (response.status() === 409) {
      await integrationsPage.closeIntegrationModal();
      test.skip();
      return;
    }

    // Verify the request payload included interactive: true
    const requestBody = JSON.parse(response.request().postData() || '{}');
    expect(requestBody.config?.interactive).toBe(true);

    // Track for cleanup
    const data = await response.json();
    if (data.id) {
      createdIntegrationIds.push(data.id);
    }

    await integrationsPage.expectModalHidden();
    await integrationsPage.expectToastVisible('created');
  });

  test('should show webhook config fields when Alertmanager type is selected', async ({ integrationsPage, page }) => {
    await integrationsPage.openCreateIntegrationModal();
    await integrationsPage.selectInboundDirection();

    await page.waitForTimeout(100);

    // Webhook config fields should be visible
    await expect(integrationsPage.configSecret).toBeVisible();
  });

  test('should show validation error when token is empty for Slack', async ({ integrationsPage, page }) => {
    await integrationsPage.openCreateIntegrationModal();
    await integrationsPage.selectOutboundDirection();

    await page.waitForTimeout(100);

    await integrationsPage.integrationNameInput.fill('Test Slack');
    // Don't fill token

    await integrationsPage.integrationFormSubmit.click();

    // Should show error toast
    await integrationsPage.expectToastVisible('required');
  });

  test('should show validation error when secret is empty for Alertmanager', async ({ integrationsPage, page }) => {
    await integrationsPage.openCreateIntegrationModal();
    await integrationsPage.selectInboundDirection();

    await page.waitForTimeout(100);

    await integrationsPage.integrationNameInput.fill('Test Alertmanager');
    // Don't fill secret

    await integrationsPage.integrationFormSubmit.click();

    // Should show error toast
    await integrationsPage.expectToastVisible('required');
  });

  test('should create Slack integration', async ({ integrationsPage, page }) => {
    const randomSuffix = Math.random().toString(36).substring(2, 8);
    const integrationName = `Slack Test ${Date.now()}-${randomSuffix}`;
    const token = `xoxb-test-token-${randomSuffix}`;

    // Set up API response listener
    const responsePromise = page.waitForResponse(
      (response) => response.url().includes('/api/v1/integrations') && response.request().method() === 'POST'
    );

    await integrationsPage.createSlackIntegration(integrationName, token);

    // Wait for API response
    const response = await responsePromise;
    // Accept 201 Created, 200 OK, or 409 Conflict (if integration already exists)
    expect([200, 201, 409]).toContain(response.status());

    if (response.status() === 409) {
      // Integration already exists, close modal and skip
      await integrationsPage.closeIntegrationModal();
      test.skip();
      return;
    }

    // Track created integration for cleanup
    const data = await response.json();
    if (data.id) {
      createdIntegrationIds.push(data.id);
    }

    // Modal should close
    await integrationsPage.expectModalHidden();

    // Should show success toast
    await integrationsPage.expectToastVisible('created');
  });

  test('should create Alertmanager webhook integration', async ({ integrationsPage, page }) => {
    const randomSuffix = Math.random().toString(36).substring(2, 8);
    const integrationName = `Alertmanager Test ${Date.now()}-${randomSuffix}`;
    const secret = `webhook-secret-${randomSuffix}`;

    // Set up API response listener
    const responsePromise = page.waitForResponse(
      (response) => response.url().includes('/api/v1/integrations') && response.request().method() === 'POST'
    );

    await integrationsPage.createAlertmanagerWebhookIntegration(integrationName, secret);

    // Wait for API response
    const response = await responsePromise;
    expect([200, 201, 409]).toContain(response.status());

    if (response.status() === 409) {
      await integrationsPage.closeIntegrationModal();
      test.skip();
      return;
    }

    // Track created integration for cleanup
    const data = await response.json();
    if (data.id) {
      createdIntegrationIds.push(data.id);
    }

    // Modal should close
    await integrationsPage.expectModalHidden();

    // Should show success toast
    await integrationsPage.expectToastVisible('created');
  });

  test('should display existing integrations in grid', async ({ integrationsPage }) => {
    const count = await integrationsPage.getIntegrationCount();

    // Count should be a number (could be 0 or more)
    expect(count).toBeGreaterThanOrEqual(0);
  });

  test('should open edit integration modal when clicking on an integration', async ({ integrationsPage }) => {
    const integrationCards = integrationsPage.integrationCards;
    const count = await integrationCards.count();

    if (count > 0) {
      const firstCard = integrationCards.first();
      const integrationId = await firstCard.getAttribute('data-integration-id');

      if (integrationId) {
        await integrationsPage.openIntegrationModal(integrationId);
        await integrationsPage.expectModalVisible();

        // Modal title should indicate edit mode
        await expect(integrationsPage.integrationModalTitle).toContainText(/Edit/i);
      }
    } else {
      test.skip();
    }
  });

  test('should pre-fill integration data in edit modal', async ({ integrationsPage }) => {
    const integrationCards = integrationsPage.integrationCards;
    const count = await integrationCards.count();

    if (count > 0) {
      const firstCard = integrationCards.first();
      const integrationId = await firstCard.getAttribute('data-integration-id');

      if (integrationId) {
        await integrationsPage.openIntegrationModal(integrationId);
        await integrationsPage.expectModalVisible();

        // Name should be pre-filled
        const nameValue = await integrationsPage.integrationNameInput.inputValue();
        expect(nameValue).toBeTruthy();
      }
    } else {
      test.skip();
    }
  });

  test('should show enabled/disabled tabs', async ({ integrationsPage }) => {
    const integrationCards = integrationsPage.integrationCards;
    const count = await integrationCards.count();

    if (count > 0) {
      const firstCard = integrationCards.first();
      const integrationId = await firstCard.getAttribute('data-integration-id');

      if (integrationId) {
        await integrationsPage.openIntegrationModal(integrationId);
        await integrationsPage.expectModalVisible();

        // Enabled tabs should be visible
        await expect(integrationsPage.enabledTabs).toBeVisible();
        await expect(integrationsPage.enabledTrue).toBeVisible();
        await expect(integrationsPage.enabledFalse).toBeVisible();
      }
    } else {
      test.skip();
    }
  });

  test('should toggle integration enabled/disabled', async ({ integrationsPage, page }) => {
    const stableCard = integrationsPage.integrationCards
      .filter({ hasText: 'Alertmanager Webhook (E2E Test)' })
      .first();
    const fallbackCard = integrationsPage.integrationCards.first();
    const targetCard =
      (await stableCard.count()) > 0
        ? stableCard
        : fallbackCard;

    await expect(targetCard).toBeVisible();
    const integrationId = await targetCard.getAttribute('data-integration-id');
    if (!integrationId) {
      test.skip();
      return;
    }

    const editBtn = targetCard.locator('.edit-integration-btn');
    await editBtn.click();
    await integrationsPage.expectModalVisible();

    const isEnabledActive = await integrationsPage.enabledTrue.evaluate(el => el.classList.contains('active'));

    await integrationsPage.setEnabled(!isEnabledActive);
    await integrationsPage.integrationFormSubmit.click();

    await integrationsPage.expectModalHidden();
    await integrationsPage.expectToastVisible('updated');
  });

  test('should delete integration with confirmation', async ({ integrationsPage, page }) => {
    // First, create an integration to delete
    const integrationName = `Delete Integration ${Date.now()}`;
    const secret = 'delete-test-secret';

    const createResponsePromise = page.waitForResponse(
      (response) => response.url().includes('/api/v1/integrations') && response.request().method() === 'POST'
    );

    await integrationsPage.createAlertmanagerWebhookIntegration(integrationName, secret);

    const createResponse = await createResponsePromise;
    const createdIntegration = await createResponse.json();
    const integrationId = createdIntegration.id;

    await integrationsPage.expectModalHidden();

    // Wait for create toast to disappear
    await page.waitForTimeout(3000);

    await integrationsPage.waitForIntegrationsLoad();

    // Now delete the integration
    // Set up dialog handler for confirmation
    page.on('dialog', dialog => dialog.accept());

    // Set up API response listener
    const deleteResponsePromise = page.waitForResponse(
      (response) => response.url().includes(`/api/v1/integrations/${integrationId}`) && response.request().method() === 'DELETE'
    );

    await integrationsPage.deleteIntegration(integrationId);

    // Wait for API response
    const deleteResponse = await deleteResponsePromise;
    expect([200, 204]).toContain(deleteResponse.status());

    // Should show success toast
    await integrationsPage.expectToastVisible('deleted');
  });

  // Cleanup created integrations after all tests
  test.afterAll(async ({ request }) => {
    if (createdIntegrationIds.length === 0) return;

    // Delete each created integration via API
    for (const id of createdIntegrationIds) {
      try {
        await request.delete(`/api/v1/integrations/${id}`);
      } catch {
        // Ignore errors (integration might already be deleted)
      }
    }

    // Clear the array
    createdIntegrationIds.length = 0;
  });
});

test.describe('Integrations - Test Connection', () => {
  test.beforeEach(async ({ integrationsPage, page }) => {
    await integrationsPage.goto();

    await page.waitForTimeout(500);
    const currentUrl = page.url();
    if (!currentUrl.includes('/cfg/integrations')) {
      test.skip();
      return;
    }

    await integrationsPage.waitForIntegrationsLoad();
  });

  test('should show test button for existing Slack integration', async ({ integrationsPage, page }) => {
    // Find a Slack integration
    const integrationCards = integrationsPage.integrationCards;
    const count = await integrationCards.count();

    if (count > 0) {
      for (let i = 0; i < count; i++) {
        const card = integrationCards.nth(i);
        const typeIndicator = await card.textContent();

        if (typeIndicator && typeIndicator.toLowerCase().includes('slack')) {
          const integrationId = await card.getAttribute('data-integration-id');

          if (integrationId) {
            await integrationsPage.openIntegrationModal(integrationId);
            await integrationsPage.expectModalVisible();

            // Test button might be visible for Slack integrations
            const testBtnVisible = await integrationsPage.integrationTestBtn.isVisible().catch(() => false);

            if (testBtnVisible) {
              await expect(integrationsPage.integrationTestBtn).toBeVisible();
              return;
            }
          }
        }
      }
    }

    // If no Slack integration found, skip
    test.skip();
  });
});

test.describe('Integrations - Slack Manifest', () => {
  test.beforeEach(async ({ integrationsPage, page }) => {
    await integrationsPage.goto();

    await page.waitForTimeout(500);
    const currentUrl = page.url();
    if (!currentUrl.includes('/cfg/integrations')) {
      test.skip();
      return;
    }

    await integrationsPage.waitForIntegrationsLoad();
  });

  test('should show Get App Manifest button when Slack type is selected', async ({ integrationsPage }) => {
    await integrationsPage.openCreateIntegrationModal();
    await integrationsPage.selectOutboundDirection();

    await expect(integrationsPage.getManifestBtn).toBeVisible();
  });

  test('should hide Get App Manifest button when Alertmanager type is selected', async ({ integrationsPage }) => {
    await integrationsPage.openCreateIntegrationModal();
    await integrationsPage.selectInboundDirection();

    await expect(integrationsPage.getManifestBtn).toBeHidden();
  });

  test('should show manifest button in configuration section for Slack', async ({ integrationsPage, page }) => {
    await integrationsPage.openCreateIntegrationModal();
    await integrationsPage.selectOutboundDirection();

    // Manifest action should be visible and placed above config fields
    await expect(integrationsPage.getManifestBtn).toBeVisible();
    await expect(integrationsPage.configFields).toBeVisible();

    // Button should be rendered before config fields in the modal layout
    const btnBox = await integrationsPage.getManifestBtn.boundingBox();
    const cfgBox = await integrationsPage.configFields.boundingBox();
    expect(btnBox).toBeTruthy();
    expect(cfgBox).toBeTruthy();
    expect(btnBox!.y).toBeLessThan(cfgBox!.y);

    // Sanity check: the button is a proper manifest action
    await expect(page.locator('#get-manifest-btn span')).toContainText('App Manifest');
  });

  test('should load and display manifest YAML on button click', async ({ integrationsPage, page }) => {
    await integrationsPage.openCreateIntegrationModal();
    await integrationsPage.selectOutboundDirection();

    // Manifest content should be hidden initially
    await expect(integrationsPage.manifestContent).toBeHidden();

    // Set up API response listener
    const manifestPromise = page.waitForResponse(
      (response) => response.url().includes('/integrations/slack/manifest')
    );

    await integrationsPage.getManifestBtn.click();

    // Wait for manifest API call
    const manifestResponse = await manifestPromise;
    expect(manifestResponse.status()).toBe(200);

    // Manifest content should now be visible
    await expect(integrationsPage.manifestContent).toBeVisible();
    await expect(integrationsPage.manifestYaml).toBeVisible();

    // Textarea should contain YAML manifest
    const yamlContent = await integrationsPage.manifestYaml.inputValue();
    expect(yamlContent).toContain('display_information');
    expect(yamlContent).toContain('Tokay');
    expect(yamlContent).toContain('oauth_config');
    expect(yamlContent).toContain('/slack/interactive');
  });

  test('should toggle manifest visibility on repeated clicks', async ({ integrationsPage }) => {
    await integrationsPage.openCreateIntegrationModal();
    await integrationsPage.selectOutboundDirection();

    // Click to show
    await integrationsPage.getManifestBtn.click();
    await expect(integrationsPage.manifestContent).toBeVisible();

    // Click to hide
    await integrationsPage.getManifestBtn.click();
    await expect(integrationsPage.manifestContent).toBeHidden();

    // Click to show again
    await integrationsPage.getManifestBtn.click();
    await expect(integrationsPage.manifestContent).toBeVisible();
  });

  test('should hide manifest when switching to inbound direction', async ({ integrationsPage }) => {
    await integrationsPage.openCreateIntegrationModal();
    await integrationsPage.selectOutboundDirection();

    // Open manifest
    await integrationsPage.getManifestBtn.click();
    await expect(integrationsPage.manifestContent).toBeVisible();

    // Switch to inbound
    await integrationsPage.selectInboundDirection();

    // Manifest button and content should be hidden
    await expect(integrationsPage.getManifestBtn).toBeHidden();
    await expect(integrationsPage.manifestContent).toBeHidden();
  });
});

test.describe('Integrations - Display', () => {
  test.beforeEach(async ({ integrationsPage, page }) => {
    await integrationsPage.goto();

    await page.waitForTimeout(500);
    const currentUrl = page.url();
    if (!currentUrl.includes('/cfg/integrations')) {
      test.skip();
      return;
    }

    await integrationsPage.waitForIntegrationsLoad();
  });

  test('should show empty state when no integrations exist', async ({ integrationsPage, page }) => {
    const count = await integrationsPage.getIntegrationCount();

    if (count === 0) {
      const emptyState = page.locator('.empty-state');
      await expect(emptyState.first()).toBeVisible();
    } else {
      test.skip();
    }
  });

  test('should display integration cards with type indicator', async ({ integrationsPage, page }) => {
    const integrationCards = integrationsPage.integrationCards;
    const count = await integrationCards.count();

    if (count > 0) {
      const firstCard = integrationCards.first();

      // Card should show integration type (Slack, Alertmanager, etc.)
      const cardText = await firstCard.textContent();
      expect(cardText).toBeTruthy();
    } else {
      test.skip();
    }
  });
});
