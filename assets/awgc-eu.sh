#!/bin/bash
# Copyright (c) 2026 AACH
# SPDX-License-Identifier: MIT
#
# awgc-eu.sh — «Через EU»: российские сервисы, которые надо отправлять через
# сервер EU, а не напрямую с сервера RU (сервер RU каскада).
#
# Нужно, когда сервис не пускает адрес сервера RU (например, считает подсети
# хостинга VPN), но спокойно работает с зарубежного адреса. Работает целиком на
# сервере: файлы подключения пользователей не меняются.
#
# Как устроено: набор ipset eu_force и правило в начале mangle PREROUTING,
# которое ставит трафику к этим сетям метку выхода через EU (0x1). Метка
# ставится раньше правил «напрямую» (ru, ru_dns), а те лишь завершают проход
# цепочки, не снимая её, — поэтому эти сети уходят через туннель awg1.
#
# Команды: apply | ensure | disable | status
set -u
A=/root/awg
LIST=$A/awgc-eu.txt
SET=eu_force
R=$A/awg-routing.sh
SUBNET=$(grep -oP '^CLIENT_SUBNET="\K[^"]+' $R 2>/dev/null | head -1)
FWMARK=$(grep -oP '^FWMARK="\K[^"]+' $R 2>/dev/null | head -1); FWMARK=${FWMARK:-0x1}
RULE=(-i awg0 -s "$SUBNET" -m set --match-set $SET dst -j MARK --set-mark "$FWMARK")

die() { echo "ОШИБКА: $*"; exit 1; }

load_set() {
    ipset create $SET hash:net -exist
    ipset create ${SET}_tmp hash:net -exist
    ipset flush ${SET}_tmp
    if [ -s "$LIST" ]; then
        grep -E '^[0-9]{1,3}(\.[0-9]{1,3}){3}(/[0-9]{1,2})?$' "$LIST" | sed "s/^/add ${SET}_tmp /" | ipset restore -exist
    fi
    ipset swap ${SET}_tmp $SET
    ipset destroy ${SET}_tmp
}

# Правило должно стоять ПЕРВЫМ в mangle PREROUTING: правила «напрямую» (RETURN)
# выше него не дали бы поставить метку.
ensure_rule() {
    local first
    first=$(iptables -t mangle -S PREROUTING 2>/dev/null | sed -n '2p')
    case "$first" in
        *"--match-set $SET dst"*) return 0 ;;
    esac
    while iptables -t mangle -D PREROUTING "${RULE[@]}" 2>/dev/null; do :; done
    iptables -t mangle -I PREROUTING 1 "${RULE[@]}"
}

write_units() {
    cat > /etc/systemd/system/awgc-eu.service <<EOF
[Unit]
Description=AmneziaWG Cascade: selected Russian networks via the EU server
After=awg-routing.service awgc-dns.service
Wants=awg-routing.service
PartOf=awg-routing.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=$A/awgc-eu.sh ensure

[Install]
WantedBy=multi-user.target awg-routing.service
EOF
    cat > /etc/systemd/system/awgc-eu.timer <<EOF
[Unit]
Description=AmneziaWG Cascade: keep the via-EU rule first

[Timer]
OnBootSec=2min
OnUnitActiveSec=10min

[Install]
WantedBy=timers.target
EOF
    systemctl daemon-reload
}

case "${1:-}" in
apply)
    [ -n "$SUBNET" ] || die "каскад не настроен (нет CLIENT_SUBNET в $R)"
    if [ -s "$LIST" ]; then
        write_units
        load_set
        ensure_rule
        systemctl enable awgc-eu.service >/dev/null 2>&1
        systemctl enable --now awgc-eu.timer >/dev/null 2>&1
        echo "@@NETS $(ipset list $SET 2>/dev/null | awk -F': ' '/Number of entries/{print $2}')"
    else
        "$0" disable >/dev/null
        echo "@@NETS 0"
    fi
    echo "@@OK"
    ;;
ensure)
    [ -n "$SUBNET" ] || exit 0
    [ -s "$LIST" ] || exit 0
    ipset list -n 2>/dev/null | grep -qx $SET || load_set
    ensure_rule
    ;;
disable)
    systemctl disable --now awgc-eu.timer awgc-eu.service >/dev/null 2>&1
    rm -f /etc/systemd/system/awgc-eu.service /etc/systemd/system/awgc-eu.timer
    systemctl daemon-reload
    [ -n "$SUBNET" ] && while iptables -t mangle -D PREROUTING "${RULE[@]}" 2>/dev/null; do :; done
    ipset destroy $SET 2>/dev/null
    rm -f "$LIST"
    echo "@@OK"
    ;;
status)
    echo "@@CASCADE $([ -n "$SUBNET" ] && echo 1 || echo 0)"
    echo "@@ACTIVE $(iptables -t mangle -S PREROUTING 2>/dev/null | grep -q -- "--match-set $SET dst" && echo 1 || echo 0)"
    echo "@@FIRST $(iptables -t mangle -S PREROUTING 2>/dev/null | sed -n '2p' | grep -q -- "--match-set $SET dst" && echo 1 || echo 0)"
    echo "@@NETS"; [ -f "$LIST" ] && cat "$LIST"; echo "@@END"
    ;;
*)
    echo "использование: $0 apply|ensure|disable|status"; exit 2
    ;;
esac
