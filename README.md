# court-watcher

Раз в час проверяет свободные слоты на крытых кортах Academy Tennis Daulet (Altegio, филиал 521176) и шлёт в Telegram только **новые** слоты.

## 1. Узнать токен и id кортов (5 минут)

1. Открой страницу бронирования в Chrome → DevTools → вкладка **Network**, фильтр `alteg`.
2. Обнови страницу. Найди запрос вида `.../api/v1/book_staff/521176`.
3. Из него возьми:
   - домен (`https://api.alteg.io/api/v1` или другой, например `https://n12345.alteg.io/api/v1`) → `API_BASE`, если отличается;
   - заголовок `Authorization: Bearer XXXX` → `ALTEGIO_TOKEN` (если заголовка нет — оставь пустым);
   - в ответе список кортов (`id`, `name`) → id крытых в `STAFF_IDS`, например `123,456`.

Без `STAFF_IDS` скрипт ищет корты по словам в названии (`NAME_FILTER`, по умолчанию `крыт,indoor,хард`) и, если ничего не нашёл, печатает список всех кортов.

## 2. Telegram-бот

1. @BotFather → `/newbot` → получить токен → `TG_TOKEN`.
2. Написать боту любое сообщение, открыть `https://api.telegram.org/bot<TG_TOKEN>/getUpdates`, взять `chat.id` → `TG_CHAT_ID`.

## 3. Локальная проверка

```bash
STAFF_IDS=123,456 ALTEGIO_TOKEN=... go run .
```
Без `TG_*` уведомление просто печатается в консоль. `state.json` хранит уже отправленные слоты — удали его, чтобы получить всё заново.

## 4. Запуск по расписанию

**GitHub Actions (бесплатно, без сервера):** запушь репозиторий (можно приватный), в Settings → Secrets добавь `TG_TOKEN`, `TG_CHAT_ID`, `ALTEGIO_TOKEN`, в Variables — `STAFF_IDS`. Workflow `.github/workflows/check.yml` запускается каждый час, вручную — кнопкой Run workflow. Учти: cron в Actions может опаздывать на 5–15 минут.

**Свой сервер / ноут:**
```bash
go build -o court-watcher .
crontab -e
# */15 * * * * cd /path/court-watcher && STAFF_IDS=123,456 TG_TOKEN=... TG_CHAT_ID=... ./court-watcher >> watcher.log 2>&1
```

## Переменные

| Переменная | По умолчанию | Что это |
|---|---|---|
| `COMPANY_ID` | `521176` | id филиала |
| `API_BASE` | `https://api.alteg.io/api/v1` | база API из DevTools |
| `ALTEGIO_TOKEN` | — | Bearer из запросов виджета |
| `STAFF_IDS` | — | id крытых кортов |
| `NAME_FILTER` | `крыт,indoor,хард` | поиск кортов по имени |
| `DAYS_AHEAD` | `7` | горизонт в днях |
| `TG_TOKEN`, `TG_CHAT_ID` | — | Telegram |
| `STATE_FILE` | `state.json` | память об отправленном |
