# QA Test Contour Proposal (TokayOps)

Дата: 2026-01-04

## 1) Цели
- Поднять максимально приближенный к продакшену контур тестирования.
- Отделить тестовую инфраструктуру от кода приложения (black-box подход).
- Проверять полный pipeline: webhook -> DB -> engine -> dispatcher -> provider.
- Запускать UI/E2E тесты локально (Playwright), без CI на первом этапе.

## 2) Принципы
- Тесты взаимодействуют только через HTTP и/или DB (никаких import internal).
- Приложение запускается как сервис (docker-compose), как в проде.
- Данные тестов фиксированы и воспроизводимы.
- Все инструменты тестов живут в `qa/` и могут быть вынесены в отдельный репозиторий.

## 3) Минимальный код-хук в приложении
Нужен только для того, чтобы направлять уведомления в HTTP-заглушку провайдера.

### 3.1. HTTP provider (new)
Файл: `internal/dispatcher/http_provider.go`
- Реализует интерфейс `Provider`.
- Делает POST в stub-сервер: `/send`, `/update`, `/resolve`, `/compact`.
- Ответ сохраняется как `provider_data` (строка JSON).

### 3.2. Регистрация провайдера
Файл: `cmd/tokayops/main.go`
- Если задан `HTTP_PROVIDER_URL`, регистрировать provider `http`.

### 3.3. Конфиг тестового окружения
Файл: `qa/config/tokay.test.yaml`
- В `targets` ставим `provider: http`.

Этот хук минимален и не меняет поведение в проде.

## 4) Предлагаемая структура (внутри репо)
```
qa/
  PROPOSAL.md
  docker-compose.e2e.yml
  config/
    tokay.test.yaml
  stub-provider/
    main.go
  api/
    tests/                 # API + pipeline tests (Playwright)
  ui/
    tests/                 # UI tests (Playwright)
  testdata/
    alertmanager/
      happy_path.json
      partial_update.json
      resolve.json
      dedup.json
  scripts/
    run_e2e.sh
    seed.sh
    generate_alerts.sh
```

## 5) Контур запуска (docker-compose)
Сервисы:
- `db` (Postgres)
- `app` (tokay)
- `provider-stub` (HTTP stub)

Переменные:
- `CONFIG_FILE=qa/config/tokay.test.yaml`
- `HTTP_PROVIDER_URL=http://provider-stub:9999`
- DB envs через compose

## 6) HTTP Stub Provider (контракт)
Эндпоинты:
- `POST /send` -> вернуть JSON строку (provider_data)
- `POST /update` -> вернуть JSON строку
- `POST /resolve` -> можно вернуть `{}` или пустую строку
- `POST /compact` -> можно вернуть `{}` или пустую строку

Дополнительно:
- `GET /events` -> вернуть список событий (для проверок)
- `POST /reset` -> очистить события

Событие:
```
{
  "type": "send|update|resolve|compact",
  "target_id": "C_DEVOPS",
  "dedup_key": "groupKey",
  "status": "processing|resolved|...",
  "created_at": "RFC3339"
}
```

## 7) Тесты (Playwright, один стек)
### 7.1 API + pipeline
Используем Playwright APIRequestContext:
- шлём webhook payload
- ждём состояния через API или /events stub
- проверяем, что были вызовы send/update/resolve

### 7.2 UI E2E
Базовые сценарии:
- login -> list -> open -> ack -> resolve
- timeline отображается
- schedules: создание override и отображение on-call

## 8) Генератор инцидентов
Скрипт шлёт готовые JSON payload из `qa/testdata/alertmanager/`
в `/webhook/alertmanager`.

## 9) Запуск локально
Пример:
```
docker-compose -f qa/docker-compose.e2e.yml up -d
qa/scripts/seed.sh
qa/scripts/generate_alerts.sh
cd qa && npx playwright test
```

## 10) Этапы внедрения
**Phase 0 (1-2 дня)**
- Структура `qa/`
- Stub provider
- Compose + тестовый конфиг

**Phase 1 (2-3 дня)**
- Pipeline E2E: happy path, partial update, resolve, dedup

**Phase 2 (2-4 дня)**
- API tests: alert-groups, schedules, RBAC

**Phase 3 (2-4 дня)**
- UI E2E: login, ack/resolve, timeline, schedules

## 11) Критерии успеха
- Полный pipeline проходит локально за 2-4 минуты.
- Ошибки в цепочке webhook -> provider ловятся тестами.
- UI регрессии ловятся Playwright тестами.

