#!/bin/bash
# Copyright (c) 2026 AACH
# SPDX-License-Identifier: MIT
#
# awgc-exitip.sh — второй адрес для выхода в интернет на сервере-выходе.
# Туннель (от сервера RU или транзита) приходит на основной адрес сервера, а
# трафик пользователей уходит в интернет со второго (SNAT для подсети awg0).
# Если адрес выхода заблокируют в России, основной адрес продолжит работать —
# достаточно сменить второй адрес, файлы подключения не меняются.
#
# Команды: on IP | off | apply | unapply | status
set -u
A=/root/awg
CONF=$A/awgc-exitip.conf
UNIT=/etc/systemd/system/awgc-exitip.service

die() { echo "ОШИБКА: $*"; exit 1; }
wan() { ip -4 route get 1.1.1.1 2>/dev/null | grep -oP 'dev \K\S+' | head -1; }
main_ip() { ip -4 route get 1.1.1.1 2>/dev/null | grep -oP 'src \K\S+' | head -1; }
# сеть awg0: 172.16.61.1/24 → 172.16.61.0/24
awg_net() {
    local a; a=$(sed -n -E 's/^Address[[:space:]]*=[[:space:]]*([0-9.]+\/[0-9]+).*/\1/p' /etc/amnezia/amneziawg/awg0.conf | head -1)
    [ -n "$a" ] || return 1
    awk -v c="$a" 'BEGIN{ split(c,p,"/"); split(p[1],o,"."); n=p[2];
        ip=((o[1]*256+o[2])*256+o[3])*256+o[4]; net=ip-(ip%(2^(32-n)));
        printf "%d.%d.%d.%d/%d\n", int(net/16777216)%256, int(net/65536)%256, int(net/256)%256, net%256, n }'
}
my_ip_via() { # какой адрес видит интернет при исходящем адресе $1
    local r u
    for u in https://api.ipify.org https://ifconfig.me/ip https://icanhazip.com; do
        r=$(curl -s -m 12 --interface "$1" "$u" 2>/dev/null | tr -d '[:space:]')
        [[ "$r" =~ ^[0-9.]+$ ]] && { echo "$r"; return 0; }
    done
    return 1
}

apply() {
    # shellcheck disable=SC1090
    . "$CONF"
    ip -4 addr show dev "$WAN" | grep -q "inet $IP/" || ip addr add "$IP/32" dev "$WAN"
    # всегда первым: правило NAT установщика (MASQUERADE) не должно его опередить
    while iptables -t nat -D POSTROUTING -s "$NET" -o "$WAN" -j SNAT --to-source "$IP" 2>/dev/null; do :; done
    iptables -t nat -I POSTROUTING 1 -s "$NET" -o "$WAN" -j SNAT --to-source "$IP"
}
unapply() {
    [ -f "$CONF" ] || return 0
    # shellcheck disable=SC1090
    . "$CONF"
    while iptables -t nat -D POSTROUTING -s "$NET" -o "$WAN" -j SNAT --to-source "$IP" 2>/dev/null; do :; done
    [ "${ADDED:-0}" = 1 ] && ip addr del "$IP/32" dev "$WAN" 2>/dev/null
    return 0
}

case "${1:-}" in
on)
    IP=${2:-}
    [[ "$IP" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || die "неверный IPv4-адрес"
    [ -f /etc/amnezia/amneziawg/awg0.conf ] || die "на сервере не установлен AmneziaWG"
    W=$(wan); [ -n "$W" ] || die "не удалось определить внешний интерфейс"
    M=$(main_ip)
    [ "$IP" != "$M" ] || die "это основной адрес сервера — нужен второй, выданный хостером"
    N=$(awg_net) || die "не удалось определить подсеть VPN"
    unapply; rm -f "$CONF"
    ADDED=0; ip -4 addr show dev "$W" | grep -q "inet $IP/" || ADDED=1
    umask 077
    printf 'IP=%s\nWAN=%s\nNET=%s\nADDED=%s\n' "$IP" "$W" "$N" "$ADDED" > "$CONF"
    apply
    got=$(my_ip_via "$IP" || true)
    if [ "$got" != "$IP" ]; then
        unapply; rm -f "$CONF"
        die "с адреса $IP интернет недоступен (ответ: ${got:-нет}). Проверьте, что хостер выдал этот адрес именно этому серверу и он уже работает (у некоторых хостеров второй адрес нужно сначала включить в панели)"
    fi
    cat > "$UNIT" <<EOF
[Unit]
Description=AmneziaWG Cascade: second exit address (SNAT for VPN traffic)
After=network-online.target awg-quick@awg0.service
Wants=network-online.target
PartOf=awg-quick@awg0.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/bash $A/awgc-exitip.sh apply
ExecStop=/bin/bash $A/awgc-exitip.sh unapply

[Install]
WantedBy=multi-user.target awg-quick@awg0.service
EOF
    systemctl daemon-reload
    systemctl enable awgc-exitip.service >/dev/null 2>&1
    systemctl start awgc-exitip.service >/dev/null 2>&1
    echo "@@OK"
    ;;
off)
    systemctl disable --now awgc-exitip.service >/dev/null 2>&1
    unapply
    rm -f "$UNIT" "$CONF"; systemctl daemon-reload
    echo "@@OK"
    ;;
apply) [ -f "$CONF" ] && apply; exit 0 ;;
unapply) unapply; exit 0 ;;
status)
    echo "@@MAIN $(main_ip)"
    if [ -f "$CONF" ]; then
        # shellcheck disable=SC1090
        . "$CONF"
        echo "@@IP $IP"
        echo "@@RULE $(iptables -t nat -C POSTROUTING -s "$NET" -o "$WAN" -j SNAT --to-source "$IP" 2>/dev/null && echo 1 || echo 0)"
    fi
    ;;
*) echo "использование: $0 on IP | off | status"; exit 2 ;;
esac
