import { Page, Locator, expect } from '@playwright/test';
import { waitForAppReady } from './app.utils';

export class DashboardPage {
  readonly page: Page;
  readonly mainApp: Locator;
  readonly authLoading: Locator;
  readonly sidebar: Locator;
  readonly globalHeader: Locator;
  readonly alertGroupsGrid: Locator;
  readonly userMenu: Locator;
  readonly userDropdown: Locator;
  readonly logoutButton: Locator;
  readonly profileButton: Locator;

  // State filter tabs
  readonly stateTabs: Locator;
  readonly stateTabActive: Locator;
  readonly stateTabTriggered: Locator;
  readonly stateTabAcknowledged: Locator;
  readonly stateTabResolved: Locator;
  readonly stateTabAll: Locator;

  // Severity chips
  readonly severityChips: Locator;

  // Alert modal
  readonly alertModal: Locator;
  readonly modalOverlay: Locator;
  readonly ackButton: Locator;
  readonly resolveButton: Locator;
  readonly closeModalButton: Locator;

  // Navigation (dynamic, rendered by JS)
  readonly sidebarNav: Locator;

  // Team modal
  readonly teamFormModal: Locator;

  // User modal
  readonly userModal: Locator;

  // Toast
  readonly toastContainer: Locator;

  // Loading & empty states
  readonly loadingState: Locator;
  readonly emptyState: Locator;

  // Pagination
  readonly pagination: Locator;
  readonly prevPage: Locator;
  readonly nextPage: Locator;
  readonly pageInfo: Locator;

  // Mode switching
  readonly modeSwitcher: Locator;
  readonly modeSwitcherOps: Locator;
  readonly modeSwitcherCfg: Locator;

  // Team context selector
  readonly teamContextTrigger: Locator;
  readonly teamContextDropdown: Locator;

  // View toggle
  readonly viewToggle: Locator;
  readonly viewGridBtn: Locator;
  readonly viewListBtn: Locator;

  // Manual alert modal
  readonly manualAlertModal: Locator;
  readonly manualAlertModalClose: Locator;
  readonly manualAlertTeam: Locator;
  readonly manualAlertSeverity: Locator;
  readonly manualAlertTitle: Locator;
  readonly manualAlertSubmit: Locator;
  readonly manualAlertCancel: Locator;
  readonly createAlertBtn: Locator;

  constructor(page: Page) {
    this.page = page;
    this.mainApp = page.locator('#main-app');
    this.authLoading = page.locator('#auth-loading');
    this.sidebar = page.locator('#sidebar');
    this.globalHeader = page.locator('#global-header');
    this.alertGroupsGrid = page.locator('#alert-groups-grid');
    this.userMenu = page.locator('#user-menu');
    this.userDropdown = page.locator('#user-dropdown');
    this.logoutButton = page.locator('#logout-btn');
    this.profileButton = page.locator('#open-profile-btn');

    // State filter tabs
    this.stateTabs = page.locator('#state-tabs');
    this.stateTabActive = page.locator('[data-state="active"]');
    this.stateTabTriggered = page.locator('[data-state="triggered"]');
    this.stateTabAcknowledged = page.locator('[data-state="acknowledged"]');
    this.stateTabResolved = page.locator('[data-state="resolved"]');
    this.stateTabAll = page.locator('[data-state="all"]');

    // Severity chips
    this.severityChips = page.locator('#severity-chips');

    // Alert modal
    this.modalOverlay = page.locator('#modal-overlay');
    this.alertModal = page.locator('#alert-group-modal');
    this.ackButton = page.locator('#action-ack');
    this.resolveButton = page.locator('#action-resolve');
    this.closeModalButton = page.locator('#modal-close');

    // Navigation
    this.sidebarNav = page.locator('#sidebar-nav');

    // Team modal
    this.teamFormModal = page.locator('#team-form-modal-overlay');

    // User modal
    this.userModal = page.locator('#user-modal-overlay');

    // Toast
    this.toastContainer = page.locator('#toast-container');

    // Loading & empty states
    this.loadingState = page.locator('#loading-state');
    this.emptyState = page.locator('#empty-state');

    // Pagination
    this.pagination = page.locator('#pagination');
    this.prevPage = page.locator('#prev-page');
    this.nextPage = page.locator('#next-page');
    this.pageInfo = page.locator('#page-info');

    // Mode switching
    this.modeSwitcher = page.locator('#header-mode-switcher');
    this.modeSwitcherOps = page.locator('[data-mode="ops"]');
    this.modeSwitcherCfg = page.locator('[data-mode="cfg"]');

    // Team context selector
    this.teamContextTrigger = page.locator('#team-context-trigger');
    this.teamContextDropdown = page.locator('#team-context-dropdown');

    // View toggle
    this.viewToggle = page.locator('#view-toggle');
    this.viewGridBtn = page.locator('#view-grid');
    this.viewListBtn = page.locator('#view-list');

    // Manual alert modal
    this.manualAlertModal = page.locator('#manual-alert-modal-overlay');
    this.manualAlertModalClose = page.locator('#manual-alert-modal-close');
    this.manualAlertTeam = page.locator('#manual-alert-team');
    this.manualAlertSeverity = page.locator('#manual-alert-severity');
    this.manualAlertTitle = page.locator('#manual-alert-title');
    this.manualAlertSubmit = page.locator('#manual-alert-submit');
    this.manualAlertCancel = page.locator('#manual-alert-cancel');
    this.createAlertBtn = page.locator('#create-manual-alert-btn');
  }

  async goto() {
    await this.page.goto('/');
  }

  async gotoAlertGroups() {
    await this.page.goto('/#/ops/alert-groups');
    await this.waitForDashboardLoad();
    await this.waitForAlertGroupsPageReady();
  }

  async waitForDashboardLoad() {
    await waitForAppReady(this.page);
    await expect(this.modeSwitcher).toBeVisible({ timeout: 10000 });
  }

  async waitForAlertGroupsPageReady() {
    // Wait for the page to be on alert-groups route and button to render
    await this.page.waitForURL(/#\/ops\/alert-groups/, { timeout: 10000 });
    await expect(this.createAlertBtn).toBeVisible({ timeout: 10000 });
  }

  async expectDashboardVisible() {
    await expect(this.mainApp).toBeVisible();
    await expect(this.sidebar).toBeVisible();
    await expect(this.alertGroupsGrid).toBeVisible();
  }

  async expectStateTabsVisible() {
    await expect(this.stateTabs).toBeVisible();
    await expect(this.stateTabActive).toBeVisible();
    await expect(this.stateTabTriggered).toBeVisible();
    await expect(this.stateTabAcknowledged).toBeVisible();
    await expect(this.stateTabResolved).toBeVisible();
    await expect(this.stateTabAll).toBeVisible();
  }

  async filterByState(state: 'active' | 'triggered' | 'acknowledged' | 'resolved' | 'all') {
    const stateTab = this.page.locator(`[data-state="${state}"]`);
    await stateTab.click();
  }

  async expectStateTabActive(state: 'active' | 'triggered' | 'acknowledged' | 'resolved' | 'all') {
    const stateTab = this.page.locator(`[data-state="${state}"]`);
    await expect(stateTab).toHaveClass(/active/, { timeout: 10000 });
  }

  async openAlertGroup(index: number = 0) {
    const alertCards = this.alertGroupsGrid.locator('.alert-group-card');
    await alertCards.nth(index).click();
  }

  async expectAlertModalVisible() {
    await expect(this.modalOverlay).toBeVisible();
  }

  async expectAlertModalHidden() {
    await expect(this.modalOverlay).toBeHidden();
  }

  async acknowledgeAlert() {
    await this.ackButton.click();
  }

  async resolveAlert() {
    await this.resolveButton.click();
  }

  async closeAlertModal() {
    await this.closeModalButton.click();
  }

  async openUserMenu() {
    await this.page.locator('#user-avatar-btn').click();
  }

  async logout() {
    await this.openUserMenu();
    await this.logoutButton.click();
  }

  async expectToastVisible(message?: string) {
    const toast = this.toastContainer.locator('.toast');
    await expect(toast.first()).toBeVisible();
    if (message) {
      await expect(toast.first()).toContainText(message);
    }
  }

  async getAlertGroupCount(): Promise<number> {
    // Wait for loading to complete
    await this.loadingState.waitFor({ state: 'hidden', timeout: 10000 }).catch(() => {});
    const cards = this.alertGroupsGrid.locator('.alert-group-card');
    return await cards.count();
  }

  async expectLoadingComplete() {
    await expect(this.loadingState).toBeHidden({ timeout: 10000 });
  }

  async expectEmptyState() {
    await expect(this.emptyState).toBeVisible();
  }

  // Mode switching methods
  async switchToConfigureMode() {
    await this.modeSwitcherCfg.click();
    await this.page.waitForURL(/#\/cfg/);
  }

  async switchToOpsMode() {
    await this.modeSwitcherOps.click();
    await this.page.waitForURL(/#\/ops/);
  }

  async expectInOpsMode() {
    await expect(this.page).toHaveURL(/#\/ops/);
    await expect(this.modeSwitcherOps).toHaveClass(/active/, { timeout: 10000 });
  }

  async expectInConfigureMode() {
    await expect(this.page).toHaveURL(/#\/cfg/);
    await expect(this.modeSwitcherCfg).toHaveClass(/active/, { timeout: 10000 });
  }

  // Team context methods
  async openTeamContextDropdown() {
    await this.teamContextTrigger.click();
  }

  async selectTeamContext(teamId: string) {
    await this.openTeamContextDropdown();
    const teamOption = this.page.locator(`[data-team-id="${teamId}"]`);
    await teamOption.click();
  }

  async selectAllTeamsContext() {
    await this.selectTeamContext('all');
  }

  // View toggle methods
  async toggleViewMode(mode: 'grid' | 'list') {
    if (mode === 'grid') {
      await this.viewGridBtn.click();
    } else {
      await this.viewListBtn.click();
    }
  }

  async expectGridView() {
    await expect(this.viewGridBtn).toHaveClass(/active/, { timeout: 10000 });
  }

  async expectListView() {
    await expect(this.viewListBtn).toHaveClass(/active/, { timeout: 10000 });
  }

  // Pagination methods
  async goToNextPage() {
    await this.nextPage.click();
  }

  async goToPrevPage() {
    await this.prevPage.click();
  }

  async expectPaginationVisible() {
    await expect(this.pagination).toBeVisible();
  }

  async expectPaginationHidden() {
    await expect(this.pagination).toBeHidden();
  }

  // Manual alert methods
  async openManualAlertModal() {
    await this.createAlertBtn.click();
    await expect(this.manualAlertModal).toHaveClass(/active/, { timeout: 10000 });
  }

  async closeManualAlertModal() {
    await this.manualAlertCancel.click();
    await expect(this.manualAlertModal).not.toHaveClass(/active/, { timeout: 10000 });
  }

  async createManualAlert(teamId: string, severity: string, title: string) {
    await this.openManualAlertModal();
    await this.manualAlertTeam.selectOption(teamId);
    await this.manualAlertSeverity.selectOption(severity);
    await this.manualAlertTitle.fill(title);
    await this.manualAlertSubmit.click();
  }

  async expectManualAlertModalVisible() {
    await expect(this.manualAlertModal).toHaveClass(/active/, { timeout: 10000 });
  }

  async expectManualAlertModalHidden() {
    await expect(this.manualAlertModal).not.toHaveClass(/active/, { timeout: 10000 });
  }

  // Navigation methods
  async navigateVia(navItem: string) {
    const navLink = this.sidebarNav.locator(`[data-route="${navItem}"]`);
    await navLink.click();
  }

  async navigateToAlertGroups() {
    await this.navigateVia('alert-groups');
  }

  async navigateToOnCall() {
    await this.navigateVia('oncall');
  }

  async navigateToTeams() {
    await this.navigateVia('teams');
  }

  async navigateToPolicies() {
    await this.navigateVia('policies');
  }

  async navigateToUsers() {
    await this.navigateVia('users');
  }

  async navigateToIntegrations() {
    await this.navigateVia('integrations');
  }

  async expectNavItemActive(navItem: string) {
    const navLink = this.sidebarNav.locator(`[data-route="${navItem}"]`);
    await expect(navLink).toHaveClass(/active/, { timeout: 10000 });
  }
}
