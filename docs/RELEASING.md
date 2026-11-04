# Как выпускать версии

Релизы на GitHub собираются автоматически: достаточно отправить в репозиторий ветку `main` и тег версии (`v1.7.0` и т. п.).

## Что происходит на GitHub

1. **Build** (`.github/workflows/build.yml`) на каждый тег `v*` собирает `AmneziaWG-Cascade.exe` из исходников, упаковывает zip, считает `SHA256SUMS.txt` и публикует релиз «AmneziaWG Cascade X.Y.Z». Описание релиза берётся из раздела этой версии в [`CHANGELOG.md`](../CHANGELOG.md) (скрипт [`scripts/release-notes.sh`](../scripts/release-notes.sh)).
2. **Release notes** (`.github/workflows/release-notes.yml`) после каждой сборки обновляет названия и описания всех релизов по `CHANGELOG.md` и отмечает самую новую версию как «Latest». Его можно запустить и вручную: Actions → Release notes → Run workflow.

## Выпуск новой версии

1. Добавить в начало `CHANGELOG.md` раздел `## X.Y.Z — ГГГГ-ММ-ДД` со списком изменений.
2. Поднять версию в `main.go` (`var version`), `winres/app.rc` (FILEVERSION, PRODUCTVERSION, строки версии) и пересобрать `rsrc_windows_amd64.syso`.
3. Закоммитить и поставить тег: `git tag -a vX.Y.Z -m "AmneziaWG Cascade X.Y.Z"`.
4. Отправить: `git push origin main` и `git push origin vX.Y.Z` — или запустить `publish.cmd` в Windows.

> Не отправляйте больше трёх тегов одной командой: GitHub в этом случае не запускает сборки. `publish.cmd` отправляет теги по одному.

Нумерация — [семантическая](https://semver.org/lang/ru/): исправления — третья цифра (1.6.0 → 1.6.1), новые возможности — вторая (1.6.1 → 1.7.0).
