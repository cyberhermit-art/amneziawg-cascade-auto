#!/bin/bash
# Copyright (c) 2026 AACH
# SPDX-License-Identifier: MIT
# release-notes.sh ВЕРСИЯ — печатает описание релиза из CHANGELOG.md
# (раздел «## ВЕРСИЯ — дата») и стандартный блок про скачивание.
# Используется в GitHub Actions; можно запустить и вручную.
set -e
V="${1#v}"
F="$(dirname "$0")/../CHANGELOG.md"
body=$(awk -v v="$V" '
  /^## / { if (on) exit; if ($2 == v) { on = 1; next } }
  on' "$F")
if [ -z "$(printf '%s' "$body" | tr -d '[:space:]')" ]; then
  body="Подробности — в CHANGELOG.md."
fi
printf '%s\n' "$body" | sed -e '/./,$!d'
CL="CHANGELOG.md"
[ -n "${GITHUB_REPOSITORY:-}" ] && CL="[CHANGELOG.md](https://github.com/$GITHUB_REPOSITORY/blob/main/CHANGELOG.md)"
REPL=""
# с 1.5.1 новая версия сама закрывает запущенную старую
[ "$(printf '%s\n1.5.1\n' "$V" | sort -V | head -1)" = "1.5.1" ] && REPL=" Если программа уже запущена, новая версия сама закроет старую."
cat <<NOTES

---

### Как скачать и запустить

1. Скачайте **AmneziaWG-Cascade-${V}-windows-x64.zip** из раздела «Assets» ниже и распакуйте.
2. Запустите \`AmneziaWG-Cascade.exe\` — установка не нужна.${REPL}
3. Windows может предупредить, что файл без цифровой подписи: «Подробнее» → «Выполнить в любом случае». Контрольные суммы — в \`SHA256SUMS.txt\`; exe собран из исходников этого релиза через GitHub Actions.

Профили и настройки сохраняются при обновлении. Полная история изменений — ${CL}.
NOTES
