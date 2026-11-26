# Коллекция Bruno — 1С:Напарник разработчика, публичный API

Коллекция собрана по официальной документации <https://code.1c.ai/public-api/> (снято 25.11.2026, статус — бета с 01.12.2026).

## Как открыть

1. Bruno → **Collection → Open Collection** → выбрать папку `bruno/1c-naparnik` (внутри лежит `bruno.json`).
2. В правом верхнем углу выбрать окружение **Local**.
3. Открыть `environments/Local.bru` и вписать свой ключ в `apiKey` (ключ берётся в разделе «Ключи доступа» на <https://code.1c.ai>).
4. Запустить `01 Диалоги / 01. Создать диалог` — `uuid` диалога автоматически попадёт в переменную `conversationUuid`, остальные запросы подхватят её сами.

## Состав

```
01 Диалоги
  01. Создать диалог                 POST   /public/v1/conversations
  02. Список диалогов                GET    /public/v1/conversations
  03. Информация о диалоге           GET    /public/v1/conversations/{uuid}
  04. Удалить диалог                 DELETE /public/v1/conversations/{uuid}
02 Название
  01. Сгенерировать название         POST   /public/v1/conversations/{uuid}/title
  02. Переименовать диалог           PUT    /public/v1/conversations/{uuid}/title
03 Контекст
  01. Токены контекста               GET    /public/v1/conversations/{uuid}/context_tokens
  02. Компактизация контекста        POST   /public/v1/conversations/{uuid}/compact
04 Сообщения
  01. Отправить сообщение (SSE)      POST   /public/v1/conversations/{uuid}/messages
  02. Сообщение с инструментами      POST   /public/v1/conversations/{uuid}/messages
  03. Результат инструмента          POST   /public/v1/conversations/{uuid}/messages
  04. Сохранить без генерации        POST   /public/v1/conversations/{uuid}/messages
05 Лимиты
  01. Бюджет и лимиты                GET    /public/v1/budget
```

## Про SSE

Все запросы `/messages` возвращают `text/event-stream`. В Bruno включите отображение потока в панели ответа. Если поток в GUI ведёт себя капризно — надёжнее через curl:

```bash
curl -N -X POST "https://code.1c.ai/public/v1/conversations/$UUID/messages" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Accept: text/event-stream" \
  -H "Content-Type: application/json" \
  -d '{"parent_uuid":null,"role":"user","content":"Привет"}'
```

Запуск всей коллекции из консоли:

```bash
npx @usebruno/cli run bruno/1c-naparnik -e Local --env-var apiKey=<ключ>
```

## Про лимиты

Абсолютные квоты (в запросах/токенах) «1С» не публикует — API отдаёт только процент использования: `sliding_5h` (скользящее 5-часовое окно) и `weekly` (неделя). Чтобы измерить «стоимость» операции, снимайте `GET /public/v1/budget` до и после типового запроса.

- `402 budget_exceeded` — исчерпан объёмный лимит;
- `402 no_active_tariff` — тариф не активирован/истёк;
- `429 rate_limit_exceeded` — слишком частые вызовы.

## Статус проверки

Все запросы, кроме `compact` и `409 conflict`, прогнаны на живом API 25.11.2026: создание диалога, два хода диалога, вызов клиентского инструмента и возврат результата, `no_generate`, генерация и переименование названия, `context_tokens`, `budget`, удаление. Подробности и расхождения с документацией — в [протоколе](../../docs/API-test-report-2026-11-25.md).

Ключевое из проверки:

- `max_context_length` фактически **384000**, а не 128000;
- `PUT /title` отвечает **204**, а не 200;
- `422` возвращает `error` **массивом**;
- в SSE-пакетах есть недокументированные поля (`type`, `details`, `diagnostics`, `usage`);
- расход лимита на 4 обращения к модели — **0.0%** (точность процента 0.1), т.е. одна операция дешевле 0.05% недельного окна.

На Windows при вызове через curl/CMD русский текст в `-d` ломает UTF-8 (`400 There was an error parsing the body`) — используйте `--data-binary @file.json` с файлом в UTF-8.

## Полный справочник

Подробное описание всех полей, моделей данных и кодов ошибок — в [docs/1C-Naparnik-Public-API.md](../../docs/1C-Naparnik-Public-API.md).
