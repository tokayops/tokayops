import { Page, Locator, expect } from '@playwright/test';
import { waitForAppReady } from './app.utils';

export class TeamsPage {
  readonly page: Page;

  // Create team modal
  readonly createTeamBtn: Locator;
  readonly teamFormModal: Locator;
  readonly teamIdInput: Locator;
  readonly teamNameInput: Locator;
  readonly teamDescInput: Locator;
  readonly teamFormSubmit: Locator;
  readonly teamFormCancel: Locator;

  // Team management modal
  readonly teamManageModal: Locator;
  readonly teamModalClose: Locator;
  readonly addMemberSelect: Locator;
  readonly addMemberRoleSelect: Locator;
  readonly addMemberBtn: Locator;
  readonly saveRoutingBtn: Locator;
  readonly deleteTeamBtn: Locator;

  // Routing configuration
  readonly routingDefaultPolicy: Locator;
  readonly routingCritical: Locator;
  readonly routingWarning: Locator;
  readonly routingInfo: Locator;
  readonly routingSection: Locator;

  // Teams grid
  readonly teamsGrid: Locator;
  readonly teamCards: Locator;
  readonly teamsLoading: Locator;

  // Toast notifications
  readonly toastContainer: Locator;

  constructor(page: Page) {
    this.page = page;

    // Create team modal elements
    this.createTeamBtn = page.locator('#create-team-view-btn');
    this.teamFormModal = page.locator('#team-form-modal-overlay');
    this.teamIdInput = page.locator('#team-form #team-id');
    this.teamNameInput = page.locator('#team-form #team-name-input');
    this.teamDescInput = page.locator('#team-form textarea#team-description');
    this.teamFormSubmit = page.locator('#team-form-submit');
    this.teamFormCancel = page.locator('#team-form-modal-close');

    // Team management modal elements
    this.teamManageModal = page.locator('#team-modal-overlay');
    this.teamModalClose = page.locator('#team-modal-close');
    this.addMemberSelect = page.locator('#team-modal-overlay #add-member-select');
    this.addMemberRoleSelect = page.locator('#team-modal-overlay #add-member-role');
    this.addMemberBtn = page.locator('#team-modal-overlay #add-member-btn');
    this.saveRoutingBtn = page.locator('#team-modal-overlay #save-routing-btn');
    this.deleteTeamBtn = page.locator('#team-modal-overlay .delete-team-modal-btn');

    // Routing configuration
    this.routingDefaultPolicy = page.locator('#routing-default-policy');
    this.routingCritical = page.locator('#routing-critical');
    this.routingWarning = page.locator('#routing-warning');
    this.routingInfo = page.locator('#routing-info');
    this.routingSection = page.locator('.routing-section, .routing-form, .team-modal-section').filter({ hasText: /routing|policy/i });

    // Teams grid
    this.teamsGrid = page.locator('#all-teams-grid');
    this.teamCards = page.locator('.team-card');
    this.teamsLoading = page.locator('#all-teams-loading');

    // Toast
    this.toastContainer = page.locator('#toast-container');
  }

  async goto() {
    await this.page.goto('/#/cfg/teams');
    await waitForAppReady(this.page);
  }

  async waitForTeamsLoad() {
    await waitForAppReady(this.page);
    await this.teamsLoading.waitFor({ state: 'hidden', timeout: 10000 }).catch(() => {});
    await expect(this.teamsGrid).toBeVisible({ timeout: 10000 });
  }

  async openCreateTeamModal() {
    await this.createTeamBtn.click();
    await expect(this.teamFormModal).toHaveClass(/active/, { timeout: 10000 });
  }

  async closeCreateTeamModal() {
    await this.teamFormCancel.click();
    await expect(this.teamFormModal).not.toHaveClass(/active/, { timeout: 10000 });
  }

  async createTeam(id: string, name: string, description?: string) {
    await this.openCreateTeamModal();
    await this.teamIdInput.fill(id);
    await this.teamNameInput.fill(name);
    if (description) {
      await this.teamDescInput.fill(description);
    }
    await this.teamFormSubmit.click();
  }

  async openTeamModal(teamId: string) {
    const teamCard = this.page.locator(`.team-card[data-team-id="${teamId}"]`);
    await teamCard.click();
    await expect(this.teamManageModal).toHaveClass(/active/, { timeout: 10000 });
    // Wait for modal content to render (loaded asynchronously after modal opens)
    await this.page.locator('#team-modal-overlay .team-modal-section').first()
      .waitFor({ state: 'visible', timeout: 10000 });
  }

  async closeTeamModal() {
    await this.teamModalClose.click();
    await expect(this.teamManageModal).not.toHaveClass(/active/, { timeout: 10000 });
  }

  async addMemberToTeam(userId: string, role: string = 'team_member') {
    await this.addMemberSelect.selectOption(userId);
    await this.addMemberRoleSelect.selectOption(role);
    await this.addMemberBtn.click();
  }

  async removeMemberFromTeam(userId: string) {
    const removeBtn = this.page.locator(`.remove-member-btn[data-user-id="${userId}"]`);
    await removeBtn.click();
  }

  async saveRouting() {
    await this.saveRoutingBtn.click();
  }

  async setDefaultPolicy(policyId: string) {
    await this.routingDefaultPolicy.selectOption(policyId);
  }

  async setCriticalPolicy(policyId: string) {
    await this.routingCritical.selectOption(policyId);
  }

  async setWarningPolicy(policyId: string) {
    await this.routingWarning.selectOption(policyId);
  }

  async setInfoPolicy(policyId: string) {
    await this.routingInfo.selectOption(policyId);
  }

  async getDefaultPolicyValue(): Promise<string> {
    return await this.routingDefaultPolicy.inputValue();
  }

  async getCriticalPolicyValue(): Promise<string> {
    return await this.routingCritical.inputValue();
  }

  async getWarningPolicyValue(): Promise<string> {
    return await this.routingWarning.inputValue();
  }

  async getInfoPolicyValue(): Promise<string> {
    return await this.routingInfo.inputValue();
  }

  async isRoutingSectionVisible(): Promise<boolean> {
    return await this.routingSection.first().isVisible().catch(() => false);
  }

  async deleteTeam() {
    await this.deleteTeamBtn.click();
  }

  async getTeamCount(): Promise<number> {
    await this.waitForTeamsLoad();
    return await this.teamCards.count();
  }

  async expectTeamExists(teamId: string) {
    const teamCard = this.page.locator(`.team-card[data-team-id="${teamId}"]`);
    await expect(teamCard).toBeVisible({ timeout: 10000 });
  }

  async expectTeamNotExists(teamId: string) {
    const teamCard = this.page.locator(`.team-card[data-team-id="${teamId}"]`);
    await expect(teamCard).toBeHidden();
  }

  async expectCreateModalVisible() {
    await expect(this.teamFormModal).toHaveClass(/active/, { timeout: 10000 });
  }

  async expectManageModalVisible() {
    await expect(this.teamManageModal).toHaveClass(/active/, { timeout: 10000 });
  }

  async expectToastVisible(message?: string) {
    const toasts = this.toastContainer.locator('.toast');
    await expect(toasts.first()).toBeVisible();
    if (message) {
      await expect(toasts.filter({ hasText: message }).first()).toBeVisible();
    }
  }
}
