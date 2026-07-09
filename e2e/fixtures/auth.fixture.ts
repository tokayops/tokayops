import { test as base } from '@playwright/test';
import { LoginPage } from '../pages/login.page';
import { DashboardPage } from '../pages/dashboard.page';
import { TeamsPage } from '../pages/teams.page';
import { UsersPage } from '../pages/users.page';
import { PoliciesPage } from '../pages/policies.page';
import { IntegrationsPage } from '../pages/integrations.page';
import { SchedulesPage } from '../pages/schedules.page';

type TestFixtures = {
  loginPage: LoginPage;
  dashboardPage: DashboardPage;
  teamsPage: TeamsPage;
  usersPage: UsersPage;
  policiesPage: PoliciesPage;
  integrationsPage: IntegrationsPage;
  schedulesPage: SchedulesPage;
};

export const test = base.extend<TestFixtures>({
  loginPage: async ({ page }, use) => {
    const loginPage = new LoginPage(page);
    await use(loginPage);
  },
  dashboardPage: async ({ page }, use) => {
    const dashboardPage = new DashboardPage(page);
    await use(dashboardPage);
  },
  teamsPage: async ({ page }, use) => {
    const teamsPage = new TeamsPage(page);
    await use(teamsPage);
  },
  usersPage: async ({ page }, use) => {
    const usersPage = new UsersPage(page);
    await use(usersPage);
  },
  policiesPage: async ({ page }, use) => {
    const policiesPage = new PoliciesPage(page);
    await use(policiesPage);
  },
  integrationsPage: async ({ page }, use) => {
    const integrationsPage = new IntegrationsPage(page);
    await use(integrationsPage);
  },
  schedulesPage: async ({ page }, use) => {
    const schedulesPage = new SchedulesPage(page);
    await use(schedulesPage);
  },
});

export { expect } from '@playwright/test';
