#!/bin/bash
# Copyright (c) 2026 AACH
# SPDX-License-Identifier: MIT
#
# awgc-mtproto.sh — MTProto-прокси для Telegram на сервере RU (mtg, FakeTLS).
# Телефоны подключаются к российскому адресу сервера RU, а соединения прокси
# с серверами Telegram уходят через туннель каскада (метка 0x1 → таблица 100 →
# текущий выход): из России Telegram замедляют, а выход за границей.
# Если туннель упал, соединения к Telegram блокируются (защита каскада),
# а не идут с адреса сервера RU.
#
# Команды:
#   on ПОРТ ДОМЕН АДРЕС  — включить (АДРЕС — внешний IP сервера для ссылки)
#   off                  — выключить и убрать
#   status               — состояние для программы
#   rules-add|rules-del  — правила маршрутизации (вызывает служба)
set -u
A=/root/awg
CONF=$A/awgc-mtproto.conf
BIN=/usr/local/bin/awgc-mtg
USR=awgc-mtg
UNIT=/etc/systemd/system/awgc-mtproto.service
FWMARK=$(grep -oP '^FWMARK="\K[^"]+' $A/awg-routing.sh 2>/dev/null | head -1); FWMARK=${FWMARK:-0x1}

die() { echo "ОШИБКА: $*"; exit 1; }

rules() { # $1: -A | -D
    local op=$1 t tbl
    # Метим только соединения, которые mtg открывает сам (к Telegram): ответы
    # телефонам по входящим соединениям должны уходить обычным путём.
    for t in "mangle OUTPUT -d 127.0.0.0/8 -m owner --uid-owner $USR -j RETURN" \
             "mangle OUTPUT -m owner --uid-owner $USR -m conntrack --ctstate NEW -j CONNMARK --set-mark 0x41" \
             "mangle OUTPUT -m owner --uid-owner $USR -m connmark --mark 0x41 -j MARK --set-mark $FWMARK" \
             "nat POSTROUTING -m owner --uid-owner $USR -o awg+ -j MASQUERADE"; do
        # shellcheck disable=SC2086
        set -- $t; tbl=$1; shift
        if [ "$op" = -A ]; then iptables -t "$tbl" -C "$@" 2>/dev/null || iptables -t "$tbl" -A "$@"
        else while iptables -t "$tbl" -D "$@" 2>/dev/null; do :; done; fi
    done
}

get_bin() {
    [ -x "$BIN" ] && "$BIN" --help >/dev/null 2>&1 && return 0
    local arch url t i
    case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) die "архитектура $(uname -m) не поддерживается" ;; esac
    t=$(mktemp -d)
    for i in "" awg1; do
        if [ -n "$i" ]; then ip link show "$i" >/dev/null 2>&1 || continue; fi
        url=$(curl -s -m 30 ${i:+--interface $i} https://api.github.com/repos/9seconds/mtg/releases/latest \
              | grep -o '"browser_download_url": *"[^"]*linux-'"$arch"'\.tar\.gz"' | head -1 | cut -d'"' -f4)
        [ -n "$url" ] || continue
        curl -sL -m 180 ${i:+--interface $i} -o "$t/mtg.tgz" "$url" && tar -xzf "$t/mtg.tgz" -C "$t" 2>/dev/null || continue
        f=$(find "$t" -type f -name mtg | head -1)
        [ -n "$f" ] && install -m 755 "$f" "$BIN" && break
    done
    rm -rf "$t"
    [ -x "$BIN" ] && return 0
    echo "@@NEEDBIN $arch"; exit 3
}

# служба mtg с ограничениями (PORT и SECRET задаёт вызывающий код)
write_unit() {
cat > "$UNIT" <<EOF
[Unit]
Description=AmneziaWG Cascade: MTProto proxy for Telegram (mtg), upstream via the cascade exit
After=network-online.target awg-routing.service

[Service]
User=$USR
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
# прокси смотрит в интернет: если его взломают, он не должен видеть
# другие процессы (в их аргументах бывают секреты) и писать на диск
NoNewPrivileges=yes
ProtectProc=invisible
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
ExecStartPre=+/bin/bash $A/awgc-mtproto.sh rules-add
ExecStart=$BIN simple-run 0.0.0.0:$PORT $SECRET
ExecStopPost=+/bin/bash $A/awgc-mtproto.sh rules-del
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
}

case "${1:-}" in
on)
    PORT=${2:?}; DOMAIN=${3:?}; HOST=${4:?}
    [[ "$PORT" =~ ^[0-9]+$ ]] && [ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ] || die "неверный порт"
    [[ "$DOMAIN" =~ ^[A-Za-z0-9.-]+$ ]] || die "неверный домен"
    [ -f $A/awg-routing.sh ] || die "на сервере RU не настроен каскад"
    get_bin
    id "$USR" >/dev/null 2>&1 || useradd -r -M -s /usr/sbin/nologin "$USR" || die "не удалось создать пользователя $USR"
    OLDPORT=""; OLDDOMAIN=""; SECRET=""
    # shellcheck disable=SC1090
    [ -f "$CONF" ] && . "$CONF" && OLDPORT=$PORT_SAVED OLDDOMAIN=$DOMAIN_SAVED SECRET=$SECRET_SAVED
    # порт занят чужой программой?
    if [ "$PORT" != "$OLDPORT" ] && ss -Hltn "sport = :$PORT" 2>/dev/null | grep -q .; then
        die "порт $PORT уже занят на сервере ($(ss -Hltnp "sport = :$PORT" | grep -o 'users:(("[^"]*' | head -1 | cut -d'"' -f2)) — выберите другой"
    fi
    if [ -z "$SECRET" ] || [ "$DOMAIN" != "$OLDDOMAIN" ]; then
        SECRET=$("$BIN" generate-secret --hex "$DOMAIN" 2>/dev/null | tr -d '[:space:]')
        [ -n "$SECRET" ] || die "mtg не создал секрет"
    fi
    umask 077
    printf 'PORT_SAVED=%s\nDOMAIN_SAVED=%s\nSECRET_SAVED=%s\nHOST_SAVED=%s\n' "$PORT" "$DOMAIN" "$SECRET" "$HOST" > "$CONF"
    write_unit
    systemctl daemon-reload
    systemctl enable awgc-mtproto >/dev/null 2>&1
    systemctl restart awgc-mtproto || die "служба не запустилась: $(journalctl -u awgc-mtproto -n 5 --no-pager -o cat | tr '\n' ' ')"
    if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
        [ -n "$OLDPORT" ] && [ "$OLDPORT" != "$PORT" ] && ufw delete allow "$OLDPORT"/tcp >/dev/null 2>&1
        ufw allow "$PORT"/tcp comment 'awgc-mtproto' >/dev/null 2>&1
    fi
    sleep 2
    systemctl is-active --quiet awgc-mtproto || die "служба остановилась: $(journalctl -u awgc-mtproto -n 5 --no-pager -o cat | tr '\n' ' ')"
    echo "@@OK"
    ;;
off)
    # shellcheck disable=SC1090
    [ -f "$CONF" ] && . "$CONF"
    systemctl disable --now awgc-mtproto >/dev/null 2>&1
    rm -f "$UNIT"; systemctl daemon-reload
    rules -D
    if [ -n "${PORT_SAVED:-}" ] && command -v ufw >/dev/null; then ufw delete allow "$PORT_SAVED"/tcp >/dev/null 2>&1; fi
    rm -f "$CONF"
    echo "@@OK"
    ;;
upgrade) # служба, поставленная старой версией программы, — без ограничений
    if [ -f "$CONF" ] && [ -f "$UNIT" ] && ! grep -q '^ProtectProc=' "$UNIT"; then
        # shellcheck disable=SC1090
        . "$CONF"; PORT=$PORT_SAVED SECRET=$SECRET_SAVED
        write_unit; systemctl daemon-reload; systemctl try-restart awgc-mtproto
    fi
    ;;
rules-add) rules -A ;;
rules-del) rules -D ;;
status)
    echo "@@ACTIVE $(systemctl is-active awgc-mtproto 2>/dev/null)"
    if [ -f "$CONF" ]; then
        # shellcheck disable=SC1090
        . "$CONF"
        echo "@@PORT $PORT_SAVED"; echo "@@DOMAIN $DOMAIN_SAVED"; echo "@@SECRET $SECRET_SAVED"; echo "@@HOST $HOST_SAVED"
        L="tg://proxy?server=$HOST_SAVED&port=$PORT_SAVED&secret=$SECRET_SAVED"
        echo "@@LINK $L"
        command -v qrencode >/dev/null && echo "@@QR $(qrencode -t PNG -s 8 -m 2 -o - "$L" 2>/dev/null | base64 -w0)"
        echo "@@CONNS $(ss -Htn state established "sport = :$PORT_SAVED" 2>/dev/null | wc -l)"
    fi
    ;;
*) echo "использование: $0 on ПОРТ ДОМЕН АДРЕС | off | status"; exit 2 ;;
esac
