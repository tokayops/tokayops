import { Page, Locator, expect } from '@playwright/test';
import { waitForAppReady } from './app.utils';

export class IntegrationsPage {
  readonly page: Page;

  // Integration modal
  readonly createIntegrationBtn: Locator;
  readonly integrationModal: Locator;
  readonly integrationModalTitle: Locator;
  readonly integrationModalClose: Locator;
  readonly integrationNameInput: Locator;
  readonly integrationTypeSelect: Locator;
  readonly integrationEnabledInput: Locator;
  readonly integrationDirectionInput: Locator;
  readonly integrationFormSubmit: Locator;
  readonly integrationFormCancel: Locator;
  readonly integrationTestBtn: Locator;

  // Direction tabs (in create mode)
  readonly directionTabs: Locator;
  readonly directionOutbound: Locator;
  readonly directionInbound: Locator;

  // Enabled tabs
  readonly enabledTabs: Locator;
  readonly enabledTrue: Locator;
  readonly enabledFalse: Locator;

  // Config fields
  readonly configFields: Locator;
  readonly configToken: Locator;
  readonly configUserToken: Locator;
  readonly configDefaultChannel: Locator;
  readonly configInteractive: Locator;
  readonly configInteractiveToggle: Locator;
  readonly configSecret: Locator;

  // Telegram config fields
  readonly configBotToken: Locator;
  readonly configSecretToken: Locator;
  readonly configDefaultChatId: Locator;

  // Manifest
  readonly getManifestBtn: Locator;
  readonly manifestContent: Locator;
  readonly manifestYaml: Locator;
  readonly copyManifestBtn: Locator;

  // Integrations grid
  readonly integrationsGrid: Locator;
  readonly integrationCards: Locator;
  readonly integrationsLoading: Locator;

  // Delivery panel
  readonly deliveriesBtn: Locator;
  readonly deliveryRows: Locator;
  readonly deliveryStatusBadge: Locator;
  readonly deliveryReplayBtn: Locator;
  readonly deliveryDetailPanel: Locator;

  // Toast notifications
  readonly toastContainer: Locator;

  constructor(page: Page) {
    this.page = page;

    // Integration modal elements
    this.createIntegrationBtn = page.locator('#create-integration-btn');
    this.integrationModal = page.locator('#integration-modal-overlay');
    this.integrationModalTitle = page.locator('#integration-modal-title');
    this.integrationModalClose = page.locator('#integration-modal-close');
    this.integrationNameInput = page.locator('#integration-name');
    this.integrationTypeSelect = page.locator('#integration-type');
    this.integrationEnabledInput = page.locator('#integration-enabled');
    this.integrationDirectionInput = page.locator('#integration-direction');
    this.integrationFormSubmit = page.locator('#integration-form-submit');
    this.integrationFormCancel = page.locator('#integration-form-cancel');
    this.integrationTestBtn = page.locator('#integration-test-btn');

    // Direction tabs
    this.directionTabs = page.locator('#direction-tabs');
    this.directionOutbound = page.locator('[data-direction="outbound"]');
    this.directionInbound = page.locator('[data-direction="inbound"]');

    // Enabled tabs
    this.enabledTabs = page.locator('#enabled-tabs');
    this.enabledTrue = page.locator('[data-enabled="true"]');
    this.enabledFalse = page.locator('[data-enabled="false"]');

    // Config fields
    this.configFields = page.locator('#integration-config-fields');
    this.configToken = page.locator('#config-token');
    this.configUserToken = page.locator('#config-user-token');
    this.configDefaultChannel = page.locator('#config-default-channel');
    this.configInteractive = page.locator('#config-interactive');
    this.configInteractiveToggle = page.locator('label.toggle-switch:has(#config-interactive)');
    this.configSecret = page.locator('#config-secret');

    // Telegram config fields
    this.configBotToken = page.locator('#config-bot-token');
    this.configSecretToken = page.locator('#config-secret-token');
    this.configDefaultChatId = page.locator('#config-default-chat-id');

    // Manifest
    this.getManifestBtn = page.locator('#get-manifest-btn');
    this.manifestContent = page.locator('#manifest-content');
    this.manifestYaml = page.locator('#manifest-yaml');
    this.copyManifestBtn = page.locator('#copy-manifest-btn');

    // Integrations grid
    this.integrationsGrid = page.locator('#integrations-grid');
    this.integrationCards = page.locator('.integration-card');
    this.integrationsLoading = page.locator('#integrations-loading');

    // Delivery panel
    this.deliveriesBtn = page.locator('.deliveries-btn');
    this.deliveryRows = page.locator('.delivery-row');
    this.deliveryStatusBadge = page.locator('.delivery-status-badge');
    this.deliveryReplayBtn = page.locator('.delivery-replay-btn');
    this.deliveryDetailPanel = page.locator('#delivery-detail-panel');

    // Toast
    this.toastContainer = page.locator('#toast-container');
  }

  async goto() {
    await this.page.goto('/#/cfg/integrations');
    await waitForAppReady(this.page);
  }

  async waitForIntegrationsLoad() {
    await waitForAppReady(this.page);
    await this.integrationsLoading.waitFor({ state: 'hidden', timeout: 10000 }).catch(() => {});
    await expect(this.integrationsGrid).toBeVisible({ timeout: 10000 });
  }

  async openCreateIntegrationModal() {
    await this.createIntegrationBtn.click();
    await expect(this.integrationModal).toHaveClass(/active/, { timeout: 10000 });
  }

  async closeIntegrationModal() {
    await this.integrationFormCancel.click();
    await expect(this.integrationModal).not.toHaveClass(/active/, { timeout: 10000 });
  }

  async selectOutboundDirection() {
    await this.directionOutbound.click();
  }

  async selectInboundDirection() {
    await this.directionInbound.click();
  }

  async setEnabled(enabled: boolean) {
    if (enabled) {
      await this.enabledTrue.click();
    } else {
      await this.enabledFalse.click();
    }
  }

  async setInteractive(enabled: boolean) {
    const current = await this.configInteractive.isChecked();
    if (current !== enabled) {
      await this.configInteractiveToggle.click();
    }
    if (enabled) {
      await expect(this.configInteractive).toBeChecked();
    } else {
      await expect(this.configInteractive).not.toBeChecked();
    }
  }

  async createSlackIntegration(name: string, token: string, userToken?: string, defaultChannel?: string) {
    await this.openCreateIntegrationModal();
    await this.selectOutboundDirection();
    await this.page.waitForTimeout(100); // Wait for type options to update
    await this.integrationNameInput.fill(name);
    await this.configToken.fill(token);
    if (userToken) {
      await this.configUserToken.fill(userToken);
    }
    if (defaultChannel) {
      await this.configDefaultChannel.fill(defaultChannel);
    }
    await this.integrationFormSubmit.click();
  }

  // openTelegramForm opens the create modal, selects outbound + telegram, and fills
  // the name + bot token. Leaves submit to the caller (so a test can omit the
  // secret to exercise validation).
  async openTelegramForm(name: string, botToken: string) {
    await this.openCreateIntegrationModal();
    await this.selectOutboundDirection();
    await this.integrationTypeSelect.selectOption('telegram');
    await this.page.waitForTimeout(100); // config fields re-render for the new type
    await this.integrationNameInput.fill(name);
    await this.configBotToken.fill(botToken);
  }

  async createTelegramIntegration(name: string, botToken: string, secretToken: string, defaultChatId?: string) {
    await this.openTelegramForm(name, botToken);
    await this.configSecretToken.fill(secretToken);
    if (defaultChatId) {
      await this.configDefaultChatId.fill(defaultChatId);
    }
    await this.integrationFormSubmit.click();
  }

  async createAlertmanagerWebhookIntegration(name: string, secret: string) {
    await this.openCreateIntegrationModal();
    await this.selectInboundDirection();
    await this.page.waitForTimeout(100); // Wait for type options to update
    await this.integrationNameInput.fill(name);
    await this.configSecret.fill(secret);
    await this.integrationFormSubmit.click();
  }

  async openIntegrationModal(integrationId: string) {
    const editBtn = this.page.locator(`.integration-card[data-integration-id="${integrationId}"] .edit-integration-btn`);
    const integrationCard = this.page.locator(`.integration-card[data-integration-id="${integrationId}"]`);

    // Prefer clicking the Edit button directly (more reliable across browsers)
    if (await editBtn.isVisible({ timeout: 2000 }).catch(() => false)) {
      await editBtn.click();
    } else {
      await integrationCard.click();
    }
    await expect(this.integrationModal).toHaveClass(/active/, { timeout: 10000 });
  }

  async editIntegration(name?: string, enabled?: boolean) {
    if (name) {
      await this.integrationNameInput.fill(name);
    }
    if (enabled !== undefined) {
      await this.setEnabled(enabled);
    }
    await this.integrationFormSubmit.click();
  }

  async deleteIntegration(integrationId: string) {
    const deleteBtn = this.page.locator(`.delete-integration-btn[data-integration-id="${integrationId}"]`);
    await deleteBtn.click();
  }

  async testIntegration() {
    await this.integrationTestBtn.click();
  }

  async getIntegrationCount(): Promise<number> {
    await this.waitForIntegrationsLoad();
    return await this.integrationCards.count();
  }

  async expectIntegrationExists(integrationId: string) {
    const integrationCard = this.page.locator(`.integration-card[data-integration-id="${integrationId}"]`);
    await expect(integrationCard).toBeVisible();
  }

  async expectIntegrationNotExists(integrationId: string) {
    const integrationCard = this.page.locator(`.integration-card[data-integration-id="${integrationId}"]`);
    await expect(integrationCard).toBeHidden();
  }

  async expectModalVisible() {
    await expect(this.integrationModal).toHaveClass(/active/, { timeout: 10000 });
  }

  async expectModalHidden() {
    await expect(this.integrationModal).not.toHaveClass(/active/, { timeout: 10000 });
  }

  async openDeliveriesPanel(integrationId: string) {
    const btn = this.page.locator(
      `.integration-card[data-integration-id="${integrationId}"] .deliveries-btn`,
    );
    await btn.click();
    await expect(this.integrationModal).toHaveClass(/active/, { timeout: 10000 });
    // Wait for async API call to render either delivery rows or empty state
    await this.page.locator('.delivery-table tbody tr, .empty-state').first()
      .waitFor({ state: 'visible', timeout: 15000 });
  }

  async expectDeliveryRowsVisible() {
    const rows = this.page.locator('.delivery-table tbody tr');
    await expect(rows.first()).toBeVisible({ timeout: 15000 });
  }

  async getDeliveryRowCount(): Promise<number> {
    return await this.page.locator('.delivery-table tbody tr').count();
  }

  async expectToastVisible(message?: string) {
    const toast = this.toastContainer.locator('.toast');
    await expect(toast.first()).toBeVisible();
    if (message) {
      await expect(toast.first()).toContainText(message);
    }
  }
}
