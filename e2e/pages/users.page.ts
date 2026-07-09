import { Page, Locator, expect } from '@playwright/test';
import { waitForAppReady } from './app.utils';

export class UsersPage {
  readonly page: Page;

  // User modal
  readonly addUserBtn: Locator;
  readonly userModal: Locator;
  readonly userModalTitle: Locator;
  readonly userModalClose: Locator;
  readonly userNameInput: Locator;
  readonly userEmailInput: Locator;
  readonly userRoleSelect: Locator;
  readonly userPasswordInput: Locator;
  readonly userPasswordResetInput: Locator;
  readonly userResetPasswordBtn: Locator;
  readonly userFormSubmit: Locator;
  readonly userFormCancel: Locator;
  readonly deleteUserBtn: Locator;

  // Users grid
  readonly usersGrid: Locator;
  readonly userRows: Locator;
  readonly usersLoading: Locator;

  // Toast notifications
  readonly toastContainer: Locator;

  constructor(page: Page) {
    this.page = page;

    // User modal elements
    this.addUserBtn = page.locator('#add-user-btn');
    this.userModal = page.locator('#user-modal-overlay');
    this.userModalTitle = page.locator('#user-modal-title');
    this.userModalClose = page.locator('#user-modal-close');
    this.userNameInput = page.locator('#user-name');
    this.userEmailInput = page.locator('#user-email');
    this.userRoleSelect = page.locator('#user-role');
    this.userPasswordInput = page.locator('#user-password');
    this.userPasswordResetInput = page.locator('#user-password-reset');
    this.userResetPasswordBtn = page.locator('#user-reset-password-btn');
    this.userFormSubmit = page.locator('#user-form-submit');
    this.userFormCancel = page.locator('#user-form-cancel');
    this.deleteUserBtn = page.locator('.delete-user-modal-btn');

    // Users grid
    this.usersGrid = page.locator('#users-grid');
    this.userRows = page.locator('.user-row');
    this.usersLoading = page.locator('#users-loading');

    // Toast
    this.toastContainer = page.locator('#toast-container');
  }

  async goto() {
    await this.page.goto('/#/cfg/users');
    await waitForAppReady(this.page);
  }

  async waitForUsersLoad() {
    await waitForAppReady(this.page);
    await this.usersLoading.waitFor({ state: 'hidden', timeout: 10000 }).catch(() => {});
    await expect(this.usersGrid).toBeVisible({ timeout: 10000 });
  }

  async openCreateUserModal() {
    await this.addUserBtn.click();
    await expect(this.userModal).toHaveClass(/active/, { timeout: 10000 });
  }

  async closeUserModal() {
    await this.userFormCancel.click();
    await expect(this.userModal).not.toHaveClass(/active/, { timeout: 10000 });
  }

  async createUser(name: string, email: string, password?: string, role?: string) {
    await this.openCreateUserModal();
    await this.userNameInput.fill(name);
    await this.userEmailInput.fill(email);
    if (password) {
      await this.userPasswordInput.fill(password);
    }
    if (role) {
      await this.userRoleSelect.selectOption(role);
    }
    await this.userFormSubmit.click();
  }

  async openUserModal(userId: string) {
    const userRow = this.page.locator(`.user-row[data-user-id="${userId}"]`);
    await userRow.click();
    await expect(this.userModal).toHaveClass(/active/, { timeout: 10000 });
  }

  async editUser(name?: string, email?: string, role?: string) {
    if (name) {
      await this.userNameInput.fill(name);
    }
    if (email) {
      await this.userEmailInput.fill(email);
    }
    if (role) {
      await this.userRoleSelect.selectOption(role);
    }
    await this.userFormSubmit.click();
  }

  async resetPassword(newPassword: string) {
    await this.userPasswordResetInput.fill(newPassword);
    await this.userResetPasswordBtn.click();
  }

  async deleteUser() {
    await this.deleteUserBtn.click();
  }

  async getUserCount(): Promise<number> {
    await this.waitForUsersLoad();
    return await this.userRows.count();
  }

  async expectUserExists(userId: string) {
    const userRow = this.page.locator(`.user-row[data-user-id="${userId}"]`);
    await expect(userRow).toBeVisible();
  }

  async expectUserNotExists(userId: string) {
    const userRow = this.page.locator(`.user-row[data-user-id="${userId}"]`);
    await expect(userRow).toBeHidden();
  }

  async expectModalVisible() {
    await expect(this.userModal).toHaveClass(/active/, { timeout: 10000 });
  }

  async expectModalHidden() {
    await expect(this.userModal).not.toHaveClass(/active/, { timeout: 10000 });
  }

  async expectToastVisible(message?: string) {
    const toasts = this.toastContainer.locator('.toast');
    await expect(toasts.first()).toBeVisible();
    if (message) {
      await expect(toasts.filter({ hasText: message }).first()).toBeVisible();
    }
  }
}
