#!/bin/bash
# Copyright (c) 2026 AACH
# SPDX-License-Identifier: MIT
# vps-update.sh — обновление системы VPS перед установкой AmneziaWG.
# Запускается программой как служба systemd (systemd-run), вывод идёт в /root/awgc-update.log.
# Пакеты только обновляются и доустанавливаются, ничего не удаляется
# (full-upgrade и autoremove на VPS иногда сносят netplan и сервер теряет сеть).
export DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a NEEDRESTART_SUSPEND=1 APT_LISTCHANGES_FRONTEND=none
DONE=${AWGC_DONE:-/root/.awgc-update-done}
# Таймауты сети: без них одно зависшее зеркало вешает apt на часы.
APT="apt-get -q -o DPkg::Lock::Timeout=1800 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 -o Acquire::ftp::Timeout=30 -o Acquire::Retries=3 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold"

echo "== Проверяю, не прервана ли прошлая установка пакетов"
dpkg --configure -a

echo "== Обновляю список пакетов"
ok=0
for i in 1 2 3; do
    # не дольше 10 минут на попытку: зависшее зеркало прерываем
    timeout -k 30 600 $APT update 2>&1 | tee /tmp/awgc-apt-update.out
    rc=${PIPESTATUS[0]}
    if [ "$rc" = 0 ]; then ok=1; break; fi
    [ "$rc" = 124 ] || [ "$rc" = 137 ] && echo "apt update завис больше 10 минут — прерываю"
    echo "Не получилось, повтор через 30 секунд ($i/3)"
    sleep 30
done
if [ "$ok" != 1 ]; then
    hosts=$(grep -hoE '^(Err|Ign):[0-9]+ https?://[^ /]+' /tmp/awgc-apt-update.out | sed -E 's#.*://##' | sort -u | paste -sd' ')
    echo "ОШИБКА: не удалось обновить список пакетов.${hosts:+ Не отвечают зеркала: $hosts.} Проверьте доступ сервера к репозиториям; если не отвечает зеркало хостера (например, ru.archive.ubuntu.com), смените его на archive.ubuntu.com в /etc/apt/sources.list.d/ или поставьте галочку «Не обновлять систему»."
    exit 1
fi

echo "== Устанавливаю обновления. На медленном канале это может занять до часа"
if ! $APT -y upgrade --with-new-pkgs; then
    echo "Первая попытка не удалась, пробую ещё раз"
    dpkg --configure -a
    $APT -y upgrade --with-new-pkgs || { echo "ОШИБКА: обновление пакетов не удалось"; exit 1; }
fi

touch "$DONE"
if [ -f /run/reboot-required ] && [ "${AWGC_NO_REBOOT:-0}" = 1 ]; then
    echo "== Обновление завершено. Для применения обновлений ядра нужна перезагрузка сервера"
elif [ -f /run/reboot-required ]; then
    echo "== Обновление завершено, нужна перезагрузка — перезагружаю сервер"
    systemd-run --quiet --on-active=5 /bin/systemctl reboot
else
    echo "== Обновление завершено, перезагрузка не нужна"
fi
