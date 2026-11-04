@echo off
rem Copyright (c) 2026 AACH
rem SPDX-License-Identifier: MIT
rem publish.cmd - отправляет программу на GitHub: ветку main и все теги версий.
rem Теги отправляются по одному: если отправить больше трёх сразу, GitHub не
rem запустит сборку релизов.
chcp 65001 >nul
setlocal
cd /d "%~dp0"

where git >nul 2>&1
if errorlevel 1 (
  echo Не найден Git. Установите Git for Windows: https://git-scm.com/download/win
  pause
  exit /b 1
)
if not exist ".git" (
  echo В этой папке нет истории git ^(скрытой папки .git^). Распакуйте архив полностью и запустите снова.
  pause
  exit /b 1
)

git remote get-url origin >nul 2>&1
if not errorlevel 1 goto have_remote
echo.
echo Укажите адрес репозитория на GitHub, например:
echo   https://github.com/ВАШ_ЛОГИН/amneziawg-cascade
set /p URL=Адрес: 
if "%URL%"=="" (
  echo Адрес не указан.
  pause
  exit /b 1
)
git remote add origin "%URL%"
:have_remote

for /f "delims=" %%u in ('git remote get-url origin') do set REMOTE=%%u
echo.
echo Репозиторий: %REMOTE%
echo ВНИМАНИЕ: содержимое репозитория на GitHub будет заменено этой версией
echo ^(правки, сделанные прямо на сайте GitHub, пропадут^).
choice /c YN /m "Продолжить"
if errorlevel 2 exit /b 0

echo.
echo == Отправляю программу ^(ветка main^)
git push --force -u origin main
if errorlevel 1 goto fail

echo.
echo == Отправляю версии ^(теги^) по одной
for /f "delims=" %%t in ('git tag -l "v*" --sort=v:refname') do (
  echo %%t
  git push --force origin "refs/tags/%%t"
  if errorlevel 1 goto fail
)

echo.
echo Готово. Сборка релизов идёт на GitHub: вкладка Actions ^(5-10 минут^),
echo затем готовые версии появятся в разделе Releases.
pause
exit /b 0

:fail
echo.
echo Не получилось. Проверьте адрес репозитория и что вы вошли в GitHub
echo ^(при первой отправке Git откроет окно входа в браузере^).
pause
exit /b 1
