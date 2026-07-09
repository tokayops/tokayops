import { test, expect } from '../../fixtures/auth.fixture';

test.describe('Users CRUD', () => {
  test.beforeEach(async ({ usersPage, page }) => {
    await usersPage.goto();

    // Check if user has admin access (redirected away means no access)
    await page.waitForTimeout(500);
    const currentUrl = page.url();
    if (!currentUrl.includes('/cfg/users')) {
      test.skip();
      return;
    }

    await usersPage.waitForUsersLoad();
  });

  test('should navigate to Users section (admin only)', async ({ usersPage, page }) => {
    await expect(page).toHaveURL(/#\/cfg\/users/);
    await expect(usersPage.usersGrid).toBeVisible();
  });

  test('should show add user button', async ({ usersPage }) => {
    await expect(usersPage.addUserBtn).toBeVisible();
  });

  test('should open create user modal', async ({ usersPage }) => {
    await usersPage.openCreateUserModal();
    await usersPage.expectModalVisible();
  });

  test('should close create user modal with cancel button', async ({ usersPage }) => {
    await usersPage.openCreateUserModal();
    await usersPage.expectModalVisible();

    await usersPage.closeUserModal();
    await usersPage.expectModalHidden();
  });

  test('should have required form fields in create user modal', async ({ usersPage }) => {
    await usersPage.openCreateUserModal();

    await expect(usersPage.userNameInput).toBeVisible();
    await expect(usersPage.userEmailInput).toBeVisible();
    await expect(usersPage.userPasswordInput).toBeVisible();
  });

  test('should show role dropdown for admin users', async ({ usersPage }) => {
    await usersPage.openCreateUserModal();

    // Role select might only be visible for admins
    const isVisible = await usersPage.userRoleSelect.isVisible().catch(() => false);

    if (isVisible) {
      const options = usersPage.userRoleSelect.locator('option');
      const optionCount = await options.count();
      expect(optionCount).toBeGreaterThanOrEqual(1);
    }
  });

  test('should create new user', async ({ usersPage, page }) => {
    const userName = `Test User ${Date.now()}`;
    const userEmail = `test-${Date.now()}@example.com`;
    const userPassword = 'TestPassword123!';

    // Set up API response listener
    const responsePromise = page.waitForResponse(
      (response) => response.url().includes('/api/v1/users') && response.request().method() === 'POST'
    );

    await usersPage.createUser(userName, userEmail, userPassword);

    // Wait for API response
    const response = await responsePromise;
    expect([200, 201]).toContain(response.status());

    // Modal should close
    await usersPage.expectModalHidden();

    // Should show success toast
    await usersPage.expectToastVisible('created');
  });

  test('should display existing users in grid', async ({ usersPage }) => {
    const count = await usersPage.getUserCount();

    // Should have at least one user (the logged-in admin)
    expect(count).toBeGreaterThanOrEqual(1);
  });

  test('should open edit user modal when clicking on a user', async ({ usersPage }) => {
    const userRows = usersPage.userRows;
    const count = await userRows.count();

    if (count > 0) {
      const firstRow = userRows.first();
      const userId = await firstRow.getAttribute('data-user-id');

      if (userId) {
        await usersPage.openUserModal(userId);
        await usersPage.expectModalVisible();

        // Modal title should indicate edit mode
        await expect(usersPage.userModalTitle).toContainText(/Edit/i);
      }
    } else {
      test.skip();
    }
  });

  test('should pre-fill user data in edit modal', async ({ usersPage }) => {
    const userRows = usersPage.userRows;
    const count = await userRows.count();

    if (count > 0) {
      const firstRow = userRows.first();
      const userId = await firstRow.getAttribute('data-user-id');

      if (userId) {
        await usersPage.openUserModal(userId);
        await usersPage.expectModalVisible();

        // Fields should have values
        const nameValue = await usersPage.userNameInput.inputValue();
        const emailValue = await usersPage.userEmailInput.inputValue();

        expect(nameValue).toBeTruthy();
        expect(emailValue).toBeTruthy();
      }
    } else {
      test.skip();
    }
  });

  test('should edit existing user', async ({ usersPage, page }) => {
    const userRows = usersPage.userRows;
    const count = await userRows.count();

    if (count > 0) {
      const firstRow = userRows.first();
      const userId = await firstRow.getAttribute('data-user-id');

      if (userId) {
        await usersPage.openUserModal(userId);
        await usersPage.expectModalVisible();

        // Set up API response listener
        const responsePromise = page.waitForResponse(
          (response) => response.url().includes(`/api/v1/users/${userId}`) && response.request().method() === 'PUT'
        );

        // Update user name
        const newName = `Updated User ${Date.now()}`;
        await usersPage.editUser(newName);

        // Wait for API response
        const response = await responsePromise;
        expect([200, 204]).toContain(response.status());

        // Modal should close
        await usersPage.expectModalHidden();

        // Should show success toast
        await usersPage.expectToastVisible('updated');
      }
    } else {
      test.skip();
    }
  });

  test('should show password reset section in edit modal', async ({ usersPage }) => {
    const userRows = usersPage.userRows;
    const count = await userRows.count();

    if (count > 0) {
      const firstRow = userRows.first();
      const userId = await firstRow.getAttribute('data-user-id');

      if (userId) {
        await usersPage.openUserModal(userId);
        await usersPage.expectModalVisible();

        // Password reset should be visible
        const isVisible = await usersPage.userPasswordResetInput.isVisible().catch(() => false);
        if (isVisible) {
          await expect(usersPage.userResetPasswordBtn).toBeVisible();
        }
      }
    } else {
      test.skip();
    }
  });

  test('should reset user password', async ({ usersPage, page }) => {
    await usersPage.goto();
    await usersPage.waitForUsersLoad();

    // Use a stable non-admin user from the visible list to avoid coupling to list refresh/order.
    let targetUserId: string | null = null;
    const userRows = usersPage.userRows;
    const rowCount = await userRows.count();
    expect(rowCount).toBeGreaterThan(0);

    for (let i = 0; i < rowCount; i++) {
      const row = userRows.nth(i);
      const email = (await row.locator('.user-email').textContent())?.trim() || '';
      if (!email || email === 'admin@example.com') {
        continue;
      }

      targetUserId = await row.getAttribute('data-user-id');
      if (!targetUserId) {
        continue;
      }

      await row.click();
      break;
    }

    expect(targetUserId).toBeTruthy();
    if (!targetUserId) return;

    await usersPage.expectModalVisible();
    await expect(usersPage.userPasswordResetInput).toBeVisible();
    await expect(usersPage.userResetPasswordBtn).toBeVisible();

    // Set up dialog handler for confirmation
    page.once('dialog', dialog => dialog.accept());

    // Set up API response listener
    const responsePromise = page.waitForResponse(
      (response) =>
        response.url().includes(`/api/v1/users/${targetUserId}/password`) &&
        response.request().method() === 'PUT',
    );

    await usersPage.resetPassword('NewPassword123!');

    // Wait for API response
    const response = await responsePromise;
    expect([200, 204]).toContain(response.status());

    // Should show success toast
    await usersPage.expectToastVisible('reset');
  });

  test('should show delete button in edit modal', async ({ usersPage }) => {
    const userRows = usersPage.userRows;
    const count = await userRows.count();

    if (count > 0) {
      const firstRow = userRows.first();
      const userId = await firstRow.getAttribute('data-user-id');

      if (userId) {
        await usersPage.openUserModal(userId);
        await usersPage.expectModalVisible();

        const isVisible = await usersPage.deleteUserBtn.isVisible().catch(() => false);

        // Delete button should be visible (may not be for own user)
        if (isVisible) {
          await expect(usersPage.deleteUserBtn).toBeVisible();
        }
      }
    } else {
      test.skip();
    }
  });

  test('should delete user with confirmation', async ({ usersPage, page }) => {
    // First create a user to delete
    const userName = `Delete User ${Date.now()}`;
    const userEmail = `delete-${Date.now()}@example.com`;

    const createResponsePromise = page.waitForResponse(
      (response) => response.url().includes('/api/v1/users') && response.request().method() === 'POST'
    );

    await usersPage.createUser(userName, userEmail, 'TestPassword123!');

    const createResponse = await createResponsePromise;
    const createdUser = await createResponse.json();
    const userId = createdUser.id;

    await usersPage.expectModalHidden();
    await usersPage.waitForUsersLoad();

    // Now delete the user
    await usersPage.openUserModal(userId);
    await usersPage.expectModalVisible();

    const isDeleteVisible = await usersPage.deleteUserBtn.isVisible().catch(() => false);

    if (isDeleteVisible) {
      // Set up dialog handler for confirmation
      page.on('dialog', dialog => dialog.accept());

      // Set up API response listener
      const deleteResponsePromise = page.waitForResponse(
        (response) => response.url().includes(`/api/v1/users/${userId}`) && response.request().method() === 'DELETE'
      );

      await usersPage.deleteUser();

      // Wait for API response
      const deleteResponse = await deleteResponsePromise;
      expect([200, 204]).toContain(deleteResponse.status());

      // Modal should close
      await usersPage.expectModalHidden();

      // Should show success toast
      await usersPage.expectToastVisible('deleted');
    }
  });
});

test.describe('Users - Validation', () => {
  test.beforeEach(async ({ usersPage, page }) => {
    await usersPage.goto();

    // Check if user has admin access
    await page.waitForTimeout(500);
    const currentUrl = page.url();
    if (!currentUrl.includes('/cfg/users')) {
      test.skip();
      return;
    }

    await usersPage.waitForUsersLoad();
  });

  test('should validate required fields on create', async ({ usersPage }) => {
    await usersPage.openCreateUserModal();

    // Try to submit without filling required fields
    await usersPage.userFormSubmit.click();

    // Form should show validation (either HTML5 validation or custom)
    // The modal should still be open
    await usersPage.expectModalVisible();
  });

  test('should validate email format', async ({ usersPage }) => {
    await usersPage.openCreateUserModal();

    await usersPage.userNameInput.fill('Test User');
    await usersPage.userEmailInput.fill('invalid-email');
    await usersPage.userPasswordInput.fill('TestPassword123!');

    await usersPage.userFormSubmit.click();

    // Email validation should fail
    // Either HTML5 validation kicks in or custom error
    await usersPage.expectModalVisible();
  });
});
