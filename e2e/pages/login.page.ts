import { Page, Locator, expect } from '@playwright/test';

export class LoginPage {
  readonly page: Page;
  readonly emailInput: Locator;
  readonly passwordInput: Locator;
  readonly submitButton: Locator;
  readonly errorMessage: Locator;
  readonly ssoSection: Locator;
  readonly ssoButton: Locator;
  readonly loginForm: Locator;

  constructor(page: Page) {
    this.page = page;
    this.emailInput = page.locator('#email');
    this.passwordInput = page.locator('#password');
    this.submitButton = page.locator('button[type="submit"]');
    this.errorMessage = page.locator('#error-message');
    this.ssoSection = page.locator('#sso-section');
    this.ssoButton = page.locator('#sso-button');
    this.loginForm = page.locator('#login-form');
  }

  async goto() {
    await this.page.goto('/login.html');
  }

  async login(email: string, password: string) {
    await this.emailInput.fill(email);
    await this.passwordInput.fill(password);
    await this.submitButton.click();
  }

  async expectErrorMessage(message: string) {
    await expect(this.errorMessage).toBeVisible();
    await expect(this.errorMessage).toContainText(message);
  }

  async expectNoError() {
    await expect(this.errorMessage).toBeHidden();
  }

  async expectSsoVisible() {
    await expect(this.ssoSection).toBeVisible();
  }

  async expectSsoHidden() {
    await expect(this.ssoSection).toBeHidden();
  }

  async expectOnLoginPage() {
    await expect(this.loginForm).toBeVisible();
    await expect(this.page).toHaveURL(/\/login\.html/);
  }
}
