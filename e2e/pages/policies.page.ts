import { Page, Locator, expect } from '@playwright/test';
import { waitForAppReady } from './app.utils';

export class PoliciesPage {
  readonly page: Page;

  // Policy modal
  readonly createPolicyBtn: Locator;
  readonly policyModal: Locator;
  readonly policyModalClose: Locator;
  readonly policyNameInput: Locator;
  readonly policyDescriptionInput: Locator;
  readonly policyTeamSelect: Locator;
  readonly policyScopeInput: Locator;
  readonly addStepBtn: Locator;
  readonly savePolicyBtn: Locator;
  readonly cancelPolicyBtn: Locator;

  // Scope tabs
  readonly scopeTabTeam: Locator;
  readonly scopeTabGlobal: Locator;

  // Policy steps
  readonly stepsList: Locator;
  readonly stepRows: Locator;

  // Policies grid
  readonly policiesGrid: Locator;
  readonly policyCards: Locator;
  readonly policiesLoading: Locator;

  // Toast notifications
  readonly toastContainer: Locator;

  constructor(page: Page) {
    this.page = page;

    // Policy modal elements
    this.createPolicyBtn = page.locator('#create-policy-btn');
    this.policyModal = page.locator('#policy-modal-overlay');
    this.policyModalClose = page.locator('#policy-modal-close');
    this.policyNameInput = page.locator('#policy-name-input');
    this.policyDescriptionInput = page.locator('#policy-description-input');
    this.policyTeamSelect = page.locator('#policy-team-select');
    this.policyScopeInput = page.locator('#policy-scope-input');
    this.addStepBtn = page.locator('#add-step-btn');
    this.savePolicyBtn = page.locator('#save-policy-btn');
    this.cancelPolicyBtn = page.locator('#cancel-policy-btn');

    // Scope tabs (in policies list view)
    this.scopeTabTeam = page.locator('.scope-tab[data-scope="team"]');
    this.scopeTabGlobal = page.locator('.scope-tab[data-scope="global"]');

    // Policy steps
    this.stepsList = page.locator('#policy-steps-list');
    this.stepRows = page.locator('.policy-step-row');

    // Policies grid
    this.policiesGrid = page.locator('#policies-grid');
    this.policyCards = page.locator('.policy-card');
    this.policiesLoading = page.locator('#policies-loading');

    // Toast
    this.toastContainer = page.locator('#toast-container');
  }

  async goto() {
    await this.page.goto('/#/cfg/policies');
    await waitForAppReady(this.page);
  }

  async waitForPoliciesLoad() {
    await waitForAppReady(this.page);
    await this.policiesLoading.waitFor({ state: 'hidden', timeout: 10000 }).catch(() => {});
    await expect(this.policiesGrid).toBeVisible({ timeout: 10000 });
  }

  async openCreatePolicyModal() {
    await this.createPolicyBtn.click();
    await expect(this.policyModal).toHaveClass(/active/, { timeout: 10000 });
  }

  async closePolicyModal() {
    await this.cancelPolicyBtn.click();
    await expect(this.policyModal).not.toHaveClass(/active/, { timeout: 10000 });
  }

  async switchToTeamScope() {
    await this.scopeTabTeam.click();
  }

  async switchToGlobalScope() {
    await this.scopeTabGlobal.click();
  }

  async createPolicy(name: string, teamId: string, description?: string) {
    await this.openCreatePolicyModal();
    await this.policyNameInput.fill(name);
    if (description) {
      await this.policyDescriptionInput.fill(description);
    }
    await this.policyTeamSelect.selectOption(teamId);
    await this.addStep(); // Add at least one step
    await this.savePolicyBtn.click();
  }

  async addStep() {
    await this.addStepBtn.click();
  }

  async configureStep(index: number, options: {
    stepType?: string;
    targetType?: string;
    targetId?: string;
    delay?: number;
    timeout?: number;
    maxAttempts?: number;
  }) {
    const row = this.stepRows.nth(index);

    if (options.stepType) {
      await row.locator('.step-type-select').selectOption(options.stepType);
    }
    if (options.targetType) {
      await row.locator('.target-type-select').selectOption(options.targetType);
    }
    if (options.targetId) {
      await row.locator('.target-id-input').fill(options.targetId);
    }
    if (options.delay !== undefined) {
      await row.locator('.delay-input').fill(String(options.delay));
    }
    if (options.timeout !== undefined) {
      await row.locator('.timeout-input').fill(String(options.timeout));
    }
    if (options.maxAttempts !== undefined) {
      await row.locator('.max-attempts-input').fill(String(options.maxAttempts));
    }
  }

  async removeStep(index: number) {
    const row = this.stepRows.nth(index);
    await row.locator('.remove-step-btn').click();
  }

  async savePolicy() {
    await this.savePolicyBtn.click();
  }

  async openPolicyModal(policyId: string) {
    const policyCard = this.page.locator(`.policy-card[data-policy-id="${policyId}"]`);
    await policyCard.click();
    await expect(this.policyModal).toHaveClass(/active/, { timeout: 10000 });
  }

  async editPolicy(policyId: string) {
    const editBtn = this.page.locator(`.edit-policy-btn[data-policy-id="${policyId}"]`);
    await editBtn.click();
    await expect(this.policyModal).toHaveClass(/active/, { timeout: 10000 });
  }

  async deletePolicy(policyId: string) {
    const deleteBtn = this.page.locator(`.delete-policy-btn[data-policy-id="${policyId}"]`);
    await deleteBtn.click();
  }

  async duplicatePolicy(policyId: string) {
    const duplicateBtn = this.page.locator(`.duplicate-policy-btn[data-policy-id="${policyId}"]`);
    await duplicateBtn.click();
  }

  async getPolicyCount(): Promise<number> {
    await this.waitForPoliciesLoad();
    return await this.policyCards.count();
  }

  async getStepCount(): Promise<number> {
    return await this.stepRows.count();
  }

  async expectPolicyExists(policyId: string) {
    const policyCard = this.page.locator(`.policy-card[data-policy-id="${policyId}"]`);
    await expect(policyCard).toBeVisible();
  }

  async expectPolicyNotExists(policyId: string) {
    const policyCard = this.page.locator(`.policy-card[data-policy-id="${policyId}"]`);
    await expect(policyCard).toBeHidden();
  }

  async expectModalVisible() {
    await expect(this.policyModal).toHaveClass(/active/, { timeout: 10000 });
  }

  async expectModalHidden() {
    await expect(this.policyModal).not.toHaveClass(/active/, { timeout: 10000 });
  }

  async expectToastVisible(message?: string) {
    const toasts = this.toastContainer.locator('.toast');
    await expect(toasts.first()).toBeVisible();
    if (message) {
      await expect(toasts.filter({ hasText: message }).first()).toBeVisible();
    }
  }
}
