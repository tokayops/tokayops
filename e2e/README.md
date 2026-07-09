# Tokay E2E Tests

End-to-end тесты для Tokay, написанные на [Playwright](https://playwright.dev/).

## Структура проекта

```
e2e/
├── fixtures/
│   └── auth.fixture.ts      # Фикстуры с Page Objects
├── pages/                   # Page Object Model
│   ├── dashboard.page.ts    # Главная страница / Dashboard
│   ├── login.page.ts        # Страница логина
│   ├── teams.page.ts        # Управление командами
│   ├── users.page.ts        # Управление пользователями
│   ├── policies.page.ts     # Эскалационные политики
│   └── integrations.page.ts # Интеграции
├── tests/
│   ├── global.setup.ts      # Глобальная настройка (аутентификация)
│   ├── auth/
│   │   └── login.spec.ts    # Тесты авторизации
│   ├── alerts/
│   │   ├── alert-actions.spec.ts  # Действия с алертами
│   │   ├── alert-filters.spec.ts  # Фильтрация алертов
│   │   └── manual-alert.spec.ts   # Создание алертов вручную
│   ├── teams/
│   │   ├── teams-crud.spec.ts     # CRUD команд
│   │   └── teams-members.spec.ts  # Управление участниками
│   ├── users/
│   │   └── users-crud.spec.ts     # CRUD пользователей
│   ├── policies/
│   │   └── policies-crud.spec.ts  # CRUD политик
│   ├── integrations/
│   │   └── integrations-crud.spec.ts # CRUD интеграций
│   ├── navigation/
│   │   └── navigation.spec.ts     # Навигация и режимы
│   └── smoke/
│       └── smoke.spec.ts          # Smoke тесты
├── playwright.config.ts     # Конфигурация Playwright
├── package.json
└── tsconfig.json
```

## Быстрый старт

### Установка зависимостей

```bash
cd e2e
npm install
npx playwright install
```

### Запуск тестов

```bash
# Все тесты
npm test

# С UI для отладки
npm run test:ui

# В headed режиме (видно браузер)
npm run test:headed

# Режим отладки
npm run test:debug

# Конкретный файл
npx playwright test tests/teams/teams-crud.spec.ts

# Конкретная группа
npx playwright test tests/teams/

# По названию теста
npx playwright test -g "should create new team"
```

### Просмотр отчёта

```bash
npm run report
```

## Конфигурация

### Переменные окружения

| Переменная | По умолчанию | Описание |
|------------|--------------|----------|
| `BASE_URL` | `http://localhost:8081` | URL приложения |
| `TEST_USER_EMAIL` | `admin@example.com` | Email тестового пользователя |
| `TEST_USER_PASSWORD` | `Admin123!` | Пароль тестового пользователя |

### Пример запуска с переменными

```bash
BASE_URL=http://localhost:3000 \
TEST_USER_EMAIL=test@test.com \
TEST_USER_PASSWORD=secret123 \
npm test
```

## Архитектура

### Page Object Model (POM)

Каждая страница приложения представлена классом с:
- **Локаторами** — селекторы элементов
- **Методами** — действия на странице
- **Assertions** — проверки состояния

```typescript
// pages/teams.page.ts
export class TeamsPage {
  readonly page: Page;
  readonly createTeamBtn: Locator;
  readonly teamCards: Locator;

  constructor(page: Page) {
    this.page = page;
    this.createTeamBtn = page.locator('#create-team-view-btn');
    this.teamCards = page.locator('.team-card');
  }

  async goto() {
    await this.page.goto('/#/cfg/teams');
  }

  async createTeam(id: string, name: string) {
    await this.openCreateTeamModal();
    await this.teamIdInput.fill(id);
    await this.teamNameInput.fill(name);
    await this.teamFormSubmit.click();
  }

  async expectTeamExists(teamId: string) {
    const card = this.page.locator(`.team-card[data-team-id="${teamId}"]`);
    await expect(card).toBeVisible();
  }
}
```

### Fixtures

Фикстуры расширяют базовый `test` объект Playwright, добавляя Page Objects:

```typescript
// fixtures/auth.fixture.ts
import { test as base } from '@playwright/test';
import { TeamsPage } from '../pages/teams.page';

export const test = base.extend<{ teamsPage: TeamsPage }>({
  teamsPage: async ({ page }, use) => {
    await use(new TeamsPage(page));
  },
});
```

Использование в тестах:

```typescript
import { test, expect } from '../../fixtures/auth.fixture';

test('should create team', async ({ teamsPage }) => {
  await teamsPage.goto();
  await teamsPage.createTeam('my-team', 'My Team');
  await teamsPage.expectTeamExists('my-team');
});
```

### Аутентификация

Аутентификация выполняется один раз в `global.setup.ts` и сохраняется в `.auth/user.json`. Все тесты используют сохранённую сессию.

```typescript
// tests/global.setup.ts
setup('authenticate', async ({ page }) => {
  const loginPage = new LoginPage(page);
  await loginPage.goto();
  await loginPage.login(email, password);
  await page.context().storageState({ path: '.auth/user.json' });
});
```

## Написание тестов

### Базовый шаблон

```typescript
import { test, expect } from '../../fixtures/auth.fixture';

test.describe('Feature Name', () => {
  test.beforeEach(async ({ dashboardPage }) => {
    // Навигация и подготовка
    await dashboardPage.goto();
    await dashboardPage.waitForDashboardLoad();
  });

  test('should do something', async ({ dashboardPage, page }) => {
    // Arrange - подготовка
    await dashboardPage.openSomeModal();

    // Act - действие
    await dashboardPage.performAction();

    // Assert - проверка
    await expect(page).toHaveURL(/expected-url/);
    await dashboardPage.expectToastVisible('Success');
  });
});
```

### Работа с API

```typescript
test('should create entity via API', async ({ page, teamsPage }) => {
  // Ожидание API ответа
  const responsePromise = page.waitForResponse(
    response => response.url().includes('/api/v1/teams')
      && response.request().method() === 'POST'
  );

  await teamsPage.createTeam('new-team', 'New Team');

  const response = await responsePromise;
  expect(response.status()).toBe(200);

  const data = await response.json();
  expect(data.id).toBe('new-team');
});
```

### Обработка диалогов (confirm)

```typescript
test('should delete with confirmation', async ({ page, teamsPage }) => {
  // Установить обработчик ДО действия
  page.on('dialog', dialog => dialog.accept());

  await teamsPage.deleteTeam();
  await teamsPage.expectToastVisible('deleted');
});
```

### Условный skip

```typescript
test('should work with existing data', async ({ teamsPage }) => {
  const count = await teamsPage.getTeamCount();

  if (count === 0) {
    test.skip(); // Пропустить если нет данных
    return;
  }

  // Тест с существующими данными
});
```

### Проверка доступа (RBAC)

```typescript
test.beforeEach(async ({ usersPage, page }) => {
  await usersPage.goto();

  // Если редирект — нет доступа
  await page.waitForTimeout(500);
  if (!page.url().includes('/cfg/users')) {
    test.skip();
    return;
  }

  await usersPage.waitForUsersLoad();
});
```

## Page Objects Reference

### DashboardPage

| Метод | Описание |
|-------|----------|
| `goto()` | Переход на главную |
| `gotoAlertGroups()` | Переход на страницу алертов |
| `waitForDashboardLoad()` | Ожидание загрузки |
| `switchToConfigureMode()` | Переключение в режим Configure |
| `switchToOpsMode()` | Переключение в режим Operations |
| `filterByState(state)` | Фильтр по состоянию алерта |
| `openAlertGroup(index)` | Открыть модалку алерта |
| `openManualAlertModal()` | Открыть форму создания алерта |
| `createManualAlert(team, severity, title)` | Создать алерт |

### TeamsPage

| Метод | Описание |
|-------|----------|
| `goto()` | Переход на страницу команд |
| `waitForTeamsLoad()` | Ожидание загрузки |
| `createTeam(id, name, desc?)` | Создать команду |
| `openTeamModal(teamId)` | Открыть модалку управления |
| `addMemberToTeam(userId, role)` | Добавить участника |
| `deleteTeam()` | Удалить команду |
| `expectTeamExists(teamId)` | Проверить наличие |

### UsersPage

| Метод | Описание |
|-------|----------|
| `goto()` | Переход на страницу пользователей |
| `createUser(name, email, password?, role?)` | Создать пользователя |
| `openUserModal(userId)` | Открыть модалку редактирования |
| `editUser(name?, email?, role?)` | Редактировать |
| `resetPassword(newPassword)` | Сбросить пароль |
| `deleteUser()` | Удалить пользователя |

### PoliciesPage

| Метод | Описание |
|-------|----------|
| `goto()` | Переход на страницу политик |
| `createPolicy(name, teamId, desc?)` | Создать политику |
| `addStep()` | Добавить шаг эскалации |
| `configureStep(index, options)` | Настроить шаг |
| `savePolicy()` | Сохранить политику |
| `deletePolicy(policyId)` | Удалить политику |

### IntegrationsPage

| Метод | Описание |
|-------|----------|
| `goto()` | Переход на страницу интеграций |
| `createSlackIntegration(name, token, ...)` | Создать Slack интеграцию |
| `createAlertmanagerWebhookIntegration(name, secret)` | Создать вебхук |
| `openIntegrationModal(id)` | Открыть модалку |
| `setEnabled(enabled)` | Включить/выключить |
| `deleteIntegration(id)` | Удалить |

## Best Practices

### 1. Используйте Page Objects

```typescript
// Плохо
await page.locator('#create-team-view-btn').click();
await page.locator('#team-id').fill('my-team');

// Хорошо
await teamsPage.openCreateTeamModal();
await teamsPage.teamIdInput.fill('my-team');
```

### 2. Изолируйте тесты

Каждый тест должен быть независимым. Создавайте данные в тесте, не полагайтесь на состояние от других тестов.

```typescript
test('should delete team', async ({ teamsPage, page }) => {
  // Сначала создать
  const teamId = `test-${Date.now()}`;
  await teamsPage.createTeam(teamId, 'Test');

  // Затем удалить
  await teamsPage.openTeamModal(teamId);
  page.on('dialog', d => d.accept());
  await teamsPage.deleteTeam();
});
```

### 3. Ждите явно, не используйте sleep

```typescript
// Плохо
await page.waitForTimeout(2000);

// Хорошо
await expect(teamsPage.teamCards).toBeVisible();
await page.waitForResponse(r => r.url().includes('/api/'));
await teamsPage.waitForTeamsLoad();
```

### 4. Используйте data-атрибуты для селекторов

```typescript
// Ненадёжно (может измениться)
page.locator('.btn.primary.large')

// Надёжно
page.locator('[data-testid="submit-btn"]')
page.locator('#create-team-btn')
page.locator('.team-card[data-team-id="my-team"]')
```

### 5. Группируйте связанные тесты

```typescript
test.describe('Teams CRUD', () => {
  test('should create team', ...);
  test('should edit team', ...);
  test('should delete team', ...);
});

test.describe('Teams - Members', () => {
  test('should add member', ...);
  test('should remove member', ...);
});
```

## Troubleshooting

### Тест падает по таймауту

1. Проверьте, что приложение запущено на `BASE_URL`
2. Убедитесь, что селекторы актуальны
3. Добавьте `--debug` для пошаговой отладки:
   ```bash
   npx playwright test tests/teams/ --debug
   ```

### Элемент не найден

1. Проверьте селектор в DevTools браузера
2. Убедитесь, что страница полностью загружена:
   ```typescript
   await teamsPage.waitForTeamsLoad();
   ```
3. Проверьте, что элемент не скрыт (другой режим, нет прав)

### Аутентификация не работает

1. Удалите `.auth/user.json` и перезапустите
2. Проверьте credentials в переменных окружения
3. Запустите setup отдельно:
   ```bash
   npx playwright test --project=setup
   ```

### Flaky тесты

1. Добавьте явные ожидания вместо таймаутов
2. Используйте `test.retry(2)` для нестабильных тестов
3. Проверьте race conditions в UI

### Просмотр trace

При падении теста автоматически сохраняется trace:

```bash
npx playwright show-trace test-results/path-to-trace.zip
```

## CI/CD

Пример для GitHub Actions:

```yaml
- name: Run E2E tests
  run: |
    cd e2e
    npm ci
    npx playwright install --with-deps
    npm test
  env:
    BASE_URL: http://localhost:8081
    TEST_USER_EMAIL: admin@example.com
    TEST_USER_PASSWORD: ${{ secrets.TEST_PASSWORD }}

- name: Upload test results
  if: failure()
  uses: actions/upload-artifact@v3
  with:
    name: playwright-report
    path: e2e/playwright-report/
```

## Полезные ссылки

- [Playwright Documentation](https://playwright.dev/docs/intro)
- [Page Object Model](https://playwright.dev/docs/pom)
- [Best Practices](https://playwright.dev/docs/best-practices)
- [Debugging](https://playwright.dev/docs/debug)
