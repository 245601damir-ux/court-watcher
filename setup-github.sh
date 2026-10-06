#!/usr/bin/env bash
# Создаёт приватный репозиторий, заливает код и прописывает secrets/variables из .env.
# Запуск: ./setup-github.sh [имя-репозитория]
set -euo pipefail
cd "$(dirname "$0")"

REPO="${1:-court-watcher}"
[ -f .env ] || { echo "Нет .env — скопируй .env.example в .env и заполни"; exit 1; }
set -a; . ./.env; set +a

: "${ALTEGIO_TOKEN:?пусто в .env}"
: "${TG_TOKEN:?пусто в .env}"
: "${TG_CHAT_ID:?пусто в .env}"

gh auth status >/dev/null

if gh repo view "$REPO" >/dev/null 2>&1; then
  echo "репозиторий $REPO уже есть, пушу в него"
  git remote get-url origin >/dev/null 2>&1 || \
    git remote add origin "$(gh repo view "$REPO" --json sshUrl -q .sshUrl)"
  git push -u origin main
else
  gh repo create "$REPO" --private --source=. --remote=origin --push
fi

SLUG="$(gh repo view "$REPO" --json nameWithOwner -q .nameWithOwner)"
echo "--- secrets ---"
printf '%s' "$ALTEGIO_TOKEN" | gh secret set ALTEGIO_TOKEN --repo "$SLUG"
printf '%s' "$TG_TOKEN"      | gh secret set TG_TOKEN      --repo "$SLUG"
printf '%s' "$TG_CHAT_ID"    | gh secret set TG_CHAT_ID    --repo "$SLUG"
[ -n "${STAFF_IDS:-}" ]  && gh variable set STAFF_IDS  --repo "$SLUG" --body "$STAFF_IDS"
[ -n "${NAME_FILTER:-}" ] && gh variable set NAME_FILTER --repo "$SLUG" --body "$NAME_FILTER"

echo "--- пробный запуск ---"
gh workflow run court-watcher --repo "$SLUG"
echo
echo "Готово: https://github.com/$SLUG/actions"
echo "Логи последнего запуска:  gh run watch --repo $SLUG"
