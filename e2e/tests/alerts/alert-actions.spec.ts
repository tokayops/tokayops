import { test, expect } from '../../fixtures/auth.fixture';
import { DashboardPage } from '../../pages/dashboard.page';

async function openAlertWithAction(
  dashboardPage: DashboardPage,
  action: 'ack' | 'resolve',
  maxChecks: number = 8,
): Promise<boolean> {
  const alertCount = await dashboardPage.getAlertGroupCount();
  const checks = Math.min(alertCount, maxChecks);
  const button = action === 'ack' ? dashboardPage.ackButton : dashboardPage.resolveButton;

  for (let i = 0; i < checks; i++) {
    await dashboardPage.openAlertGroup(i);
    await dashboardPage.expectAlertModalVisible();

    const isVisible = await button.isVisible().catch(() => false);
    if (isVisible) {
      return true;
    }

    await dashboardPage.closeAlertModal();
    await dashboardPage.expectAlertModalHidden();
  }

  return false;
}

test.describe('Alert Actions', () => {
  test.beforeEach(async ({ dashboardPage }) => {
    await dashboardPage.goto();
    await dashboardPage.waitForDashboardLoad();
    await dashboardPage.expectLoadingComplete();
  });

  test('should open alert group modal when clicking on an alert', async ({ dashboardPage }) => {
    const alertCount = await dashboardPage.getAlertGroupCount();

    if (alertCount > 0) {
      await dashboardPage.openAlertGroup(0);
      await dashboardPage.expectAlertModalVisible();
    } else {
      test.skip();
    }
  });

  test('should close alert modal with close button', async ({ dashboardPage }) => {
    const alertCount = await dashboardPage.getAlertGroupCount();

    if (alertCount > 0) {
      await dashboardPage.openAlertGroup(0);
      await dashboardPage.expectAlertModalVisible();

      await dashboardPage.closeAlertModal();
      await dashboardPage.expectAlertModalHidden();
    } else {
      test.skip();
    }
  });

  test('should close alert modal by clicking overlay', async ({ dashboardPage, page }) => {
    const alertCount = await dashboardPage.getAlertGroupCount();

    if (alertCount > 0) {
      await dashboardPage.openAlertGroup(0);
      await dashboardPage.expectAlertModalVisible();

      // Click on overlay (outside modal content)
      await page.locator('#modal-overlay').click({ position: { x: 10, y: 10 } });
      await dashboardPage.expectAlertModalHidden();
    } else {
      test.skip();
    }
  });

  test('should have action buttons in alert modal for triggered alert', async ({ dashboardPage, page }) => {
    // Filter to triggered alerts
    await dashboardPage.filterByState('triggered');
    await page.waitForTimeout(500);

    const alertCount = await dashboardPage.getAlertGroupCount();

    if (alertCount > 0) {
      // Find a triggered alert where acknowledge action is available.
      const found = await openAlertWithAction(dashboardPage, 'ack');
      expect(found).toBe(true);
    } else {
      test.skip();
    }
  });

  test('should have resolve button in alert modal', async ({ dashboardPage, page }) => {
    // Filter to acknowledged alerts
    await dashboardPage.filterByState('acknowledged');
    await page.waitForTimeout(500);

    const alertCount = await dashboardPage.getAlertGroupCount();

    if (alertCount > 0) {
      const found = await openAlertWithAction(dashboardPage, 'resolve');
      expect(found).toBe(true);
    } else {
      test.skip();
    }
  });

  test('should acknowledge alert when clicking acknowledge button', async ({ dashboardPage, page }) => {
    // Filter to triggered alerts first
    await dashboardPage.filterByState('triggered');
    await page.waitForTimeout(500);

    const alertCount = await dashboardPage.getAlertGroupCount();

    if (alertCount > 0) {
      const found = await openAlertWithAction(dashboardPage, 'ack');
      if (!found) {
        test.skip();
        return;
      }

      // Wait for the API response
      const responsePromise = page.waitForResponse(
        (response) => response.url().includes('/ack') && response.status() === 200
      );

      await dashboardPage.acknowledgeAlert();

      // Verify API call was successful
      await responsePromise;

      // Modal should still be open but status may have changed
      await dashboardPage.expectAlertModalVisible();
    } else {
      test.skip();
    }
  });

  test('should resolve alert when clicking resolve button', async ({ dashboardPage, page }) => {
    // Filter to acknowledged alerts first
    await dashboardPage.filterByState('acknowledged');
    await page.waitForTimeout(500);

    const alertCount = await dashboardPage.getAlertGroupCount();

    if (alertCount > 0) {
      const found = await openAlertWithAction(dashboardPage, 'resolve');
      if (!found) {
        test.skip();
        return;
      }

      // Wait for the API response
      const responsePromise = page.waitForResponse(
        (response) => response.url().includes('/resolve') && response.status() === 200
      );

      await dashboardPage.resolveAlert();

      // Verify API call was successful
      await responsePromise;

      // Modal should still be open but status may have changed
      await dashboardPage.expectAlertModalVisible();
    } else {
      test.skip();
    }
  });
});
