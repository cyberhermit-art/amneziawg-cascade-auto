#!/bin/bash
# Copyright (c) 2026 AACH
# SPDX-License-Identifier: MIT
# vps-update.sh — обновление системы VPS перед установкой AmneziaWG.
# Запускается программой как служба systemd (systemd-run), вывод идёт в /root/awgc-update.log.
# Пакеты только обновляются и доустанавливаются, ничего не удаляется
# (full-upgrade и autoremove на VPS иногда сносят netplan и сервер теряет сеть).
export DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a NEEDRESTART_SUSPEND=1 APT_LISTCHANGES_FRONTEND=none
DONE=/root/.awgc-update-done
APT="apt-get -q -o DPkg::Lock::Timeout=1800 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold"

echo "== Проверяю, не прервана ли прошлая установка пакетов"
dpkg --configure -a

echo "== Обновляю список пакетов"
ok=0
for i in 1 2 3; do
    if $APT update; then ok=1; break; fi
    echo "Не получилось, повтор через 30 секунд ($i/3)"
    sleep 30
done
[ "$ok" = 1 ] || { echo "ОШИБКА: не удалось обновить список пакетов (нет доступа к репозиториям?)"; exit 1; }

echo "== Устанавливаю обновления. На медленном канале это может занять до часа"
if ! $APT -y upgrade --with-new-pkgs; then
    echo "Первая попытка не удалась, пробую ещё раз"
    dpkg --configure -a
    $APT -y upgrade --with-new-pkgs || { echo "ОШИБКА: обновление пакетов не удалось"; exit 1; }
fi

touch "$DONE"
if [ -f /run/reboot-required ]; then
    echo "== Обновление завершено, нужна перезагрузка — перезагружаю сервер"
    systemd-run --quiet --on-active=5 /bin/systemctl reboot
else
    echo "== Обновление завершено, перезагрузка не нужна"
fi
