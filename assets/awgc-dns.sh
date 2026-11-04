#!/bin/bash
# Copyright (c) 2026 AACH
# SPDX-License-Identifier: MIT
#
# awgc-dns.sh — маршрутизация российских сервисов по доменам (сервер RU каскада).
#
# Списки сетей по странам не ловят российские сервисы, которые работают с адресов
# зарубежных CDN и облаков: такие адреса уходят через EU, и сервис видит заграницу.
# Здесь на сервере RU работает свой DNS (dnsmasq) для пользователей VPN:
#   - домены российских сервисов он разрешает через российский DNS (Яндекс), а их
#     адреса добавляет в набор ipset ru_dns — трафик к ним идёт напрямую через RU;
#   - остальные домены разрешаются через Cloudflare DNS по туннелю в EU.
# Пользователям VPN в файле подключения прописывается DNS = адрес сервера RU в VPN.
#
# Команды: enable | apply | disable | update | prepare | status
#   enable  — поставить dnsmasq, собрать список, запустить, перевести DNS всех пользователей;
#   apply   — пересобрать список и перезапустить (настройки поменялись);
#   disable — вернуть пользователям прежний DNS и остановить;
#   update  — обновить список доменов (раз в неделю по таймеру);
#   prepare — подготовка перед запуском службы (ipset, правило, маршруты);
#   status  — состояние для программы.
set -u
export DEBIAN_FRONTEND=noninteractive
A=/root/awg; D=/etc/awgc-dns
C=/etc/amnezia/amneziawg/awg0.conf
M=$A/manage_amneziawg.sh
I=$A/awgsetup_cfg.init
SET=ru_dns
LIST_URL="https://raw.githubusercontent.com/lib4u/amnezia-tunneling-ru/main/amnezia.json"
RU_DNS="77.88.8.8 77.88.8.1"     # Яндекс DNS: для российских доменов — российские адреса
UP_DNS="1.1.1.1 1.0.0.1"         # остальные домены — через туннель в EU
LIST=$A/awgc-dns-list.txt        # скачанный список (прежний остаётся, если новый не скачался)
CUSTOM=$A/awgc-dns-custom.txt    # свои домены пользователя
TLDS=$A/awgc-dns-tlds            # есть файл — целиком зоны .ru .рф .su
PREV=$A/awgc-dns.prev-dns        # DNS пользователей до включения

ADDR=$(sed -n -E 's/^Address[[:space:]]*=[[:space:]]*([0-9.]+)\/.*/\1/p' "$C" 2>/dev/null | head -1)
SUBNET=$(grep -oP '^CLIENT_SUBNET="\K[^"]+' $A/awg-routing.sh 2>/dev/null | head -1)

die() { echo "ОШИБКА: $*"; exit 1; }

# Главные российские сервисы — в списке всегда, даже если общий список не скачался.
BUILTIN="gosuslugi.ru gu-st.ru nalog.gov.ru mos.ru gov.ru pfr.gov.ru sfr.gov.ru
max.ru oneme.ru vk.com vk.ru vkuser.net userapi.com vk-cdn.net mail.ru ok.ru mycdn.me
yandex.ru yandex.net ya.ru yastatic.net yandex.com dzen.ru kinopoisk.ru rutube.ru
sberbank.ru sber.ru sberbank.com tbank.ru tinkoff.ru tbank-online.com vtb.ru alfabank.ru
gazprombank.ru raiffeisen.ru psbank.ru sovcombank.ru open.ru pochtabank.ru nspk.ru mironline.ru
ozon.ru ozone.ru ozon.com wildberries.ru wb.ru wbbasket.ru wbstatic.net avito.ru avito.st
mts.ru beeline.ru megafon.ru tele2.ru t2.ru rt.ru rostelecom.ru
rzd.ru aeroflot.ru hh.ru 2gis.ru cian.ru auto.ru drom.ru okko.tv ivi.ru kion.ru
kaspersky.ru kaspersky.com drweb.ru"

valid_domain() { [[ "$1" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$ ]]; }

fetch_list() {
    local t; t=$(mktemp)
    if curl -fsS --retry 2 --connect-timeout 10 --max-time 60 -o "$t" "$LIST_URL"; then
        grep -oE '"hostname"[[:space:]]*:[[:space:]]*"[^"]+"' "$t" | sed -E 's/.*"([^"]+)"$/\1/' > "$t.h"
        if [ "$(wc -l < "$t.h")" -ge 300 ]; then
            mv -f "$t.h" "$LIST"; rm -f "$t"; return 0
        fi
        echo "WARN: список доменов выглядит неполным — оставляю прежний"
    else
        echo "WARN: список доменов не скачался — оставляю прежний"
    fi
    rm -f "$t" "$t.h"; return 1
}

# Собирает итоговый список доменов в $D/domains.txt и конфиг dnsmasq.
build() {
    [ -n "$ADDR" ] || die "не найден адрес сервера в $C"
    mkdir -p "$D"
    local all; all=$(mktemp)
    { printf '%s\n' $BUILTIN; [ -f "$LIST" ] && cat "$LIST"; [ -f "$CUSTOM" ] && cat "$CUSTOM"; } \
        | tr 'A-Z' 'a-z' | sed -E 's/[[:space:]#].*$//; s/^\*?\.//; s/\.$//' \
        | grep -vE '(^|\.)(arpa|local|localdomain|localhost|lan|internal|invalid|test|example|home)$' \
        | while read -r d; do valid_domain "$d" && echo "$d"; done | sort -u > "$all"
    [ -f "$TLDS" ] && printf '%s\n' ru su xn--p1ai >> "$all"
    sort -u "$all" -o "$D/domains.txt"; rm -f "$all"
    {
        echo "# создан awgc-dns.sh — не править вручную"
        while read -r d; do
            for s in $RU_DNS; do echo "server=/$d/$s"; done
            echo "ipset=/$d/$SET"
        done < "$D/domains.txt"
    } > "$D/domains.conf.new"
    {
        echo "# DNS для пользователей VPN каскада (awgc-dns.sh)"
        echo "port=53"
        echo "listen-address=$ADDR"
        echo "bind-dynamic"
        echo "no-resolv"
        echo "no-hosts"
        echo "cache-size=10000"
        for s in $UP_DNS; do echo "server=$s"; done
        echo "conf-file=$D/domains.conf"
    } > "$D/dnsmasq.conf"
    if cmp -s "$D/domains.conf.new" "$D/domains.conf" 2>/dev/null; then rm -f "$D/domains.conf.new"; return 1; fi
    mv -f "$D/domains.conf.new" "$D/domains.conf"; return 0
}

prepare() {
    [ -n "$SUBNET" ] || die "каскад не настроен (нет CLIENT_SUBNET в awg-routing.sh)"
    ipset create $SET hash:ip timeout 21600 -exist
    # правило «Через EU» (eu_force), если есть, должно остаться первым
    local pos=1
    iptables -t mangle -S PREROUTING 2>/dev/null | sed -n '2p' | grep -q -- '--match-set eu_force dst' && pos=2
    iptables -t mangle -C PREROUTING -i awg0 -s "$SUBNET" -m set --match-set $SET dst -j RETURN 2>/dev/null \
        || iptables -t mangle -I PREROUTING $pos -i awg0 -s "$SUBNET" -m set --match-set $SET dst -j RETURN
    # запросы к общему DNS — через туннель в EU (как у пользователей без этой функции)
    if ip link show awg1 >/dev/null 2>&1; then
        for s in $UP_DNS; do ip route replace "$s" dev awg1; done
    fi
    if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q '^Status: active'; then
        ufw status | grep -q 'awgc-dns' || {
            ufw allow in on awg0 to any port 53 proto udp comment awgc-dns >/dev/null
            ufw allow in on awg0 to any port 53 proto tcp comment awgc-dns >/dev/null
        }
    fi
    return 0
}

write_units() {
    cat > /etc/systemd/system/awgc-dns.service <<EOF
[Unit]
Description=AmneziaWG Cascade: DNS for VPN users with domain routing of Russian services
After=awg-quick@awg0.service awg-quick@awg1.service awg-routing.service network-online.target
Wants=awg-routing.service
PartOf=awg-quick@awg1.service

[Service]
Type=simple
ExecStartPre=$A/awgc-dns.sh prepare
ExecStart=/usr/sbin/dnsmasq -k --conf-file=$D/dnsmasq.conf
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
    cat > /etc/systemd/system/awgc-dns-update.service <<EOF
[Unit]
Description=AmneziaWG Cascade: update Russian domain list

[Service]
Type=oneshot
ExecStart=$A/awgc-dns.sh update
EOF
    cat > /etc/systemd/system/awgc-dns-update.timer <<EOF
[Unit]
Description=AmneziaWG Cascade: weekly update of Russian domain list

[Timer]
OnCalendar=weekly
RandomizedDelaySec=6h
Persistent=true

[Install]
WantedBy=timers.target
EOF
    systemctl daemon-reload
}

# Сменить DNS всем пользователям (и новым — через awgsetup_cfg.init).
set_clients_dns() {
    local dns=$1 ok=0 bad=0 n
    touch "$I"
    if grep -qE '^[[:space:]]*(export[[:space:]]+)?CLIENT_DNS=' "$I"; then
        sed -i -E "s|^[[:space:]]*(export[[:space:]]+)?CLIENT_DNS=.*|export CLIENT_DNS='$dns'|" "$I"
    else
        echo "export CLIENT_DNS='$dns'" >> "$I"
    fi
    for n in $(grep '^#_Name = ' "$C" | sed 's/^#_Name = //'); do
        if bash "$M" --yes --no-color modify "$n" DNS "$dns" >/tmp/awgc-dns-mod.log 2>&1; then ok=$((ok+1))
        else bad=$((bad+1)); echo "@@FAIL $n $(tail -1 /tmp/awgc-dns-mod.log)"; fi
    done
    echo "@@CLIENTS $ok $bad"
}

current_dns() {
    local f; f=$(ls $A/*.conf 2>/dev/null | grep -v '/awgc-' | head -1)
    [ -n "$f" ] && sed -n -E 's/^DNS[[:space:]]*=[[:space:]]*//p' "$f" | head -1
}

check_running() {
    local i
    for i in 1 2 3 4 5 6 7 8 9 10; do
        systemctl is-active --quiet awgc-dns && ss -lun 2>/dev/null | grep -q "$ADDR:53 " && return 0
        sleep 1
    done
    return 1
}

case "${1:-}" in
enable)
    [ -n "$SUBNET" ] || die "сначала нужен настроенный каскад"
    [ -n "$ADDR" ] || die "не найден адрес сервера в $C"
    if ! [ -x /usr/sbin/dnsmasq ]; then
        echo "== Ставлю dnsmasq"
        apt-get -o DPkg::Lock::Timeout=300 -qq update >/dev/null 2>&1
        apt-get -o DPkg::Lock::Timeout=300 -y -qq install dnsmasq-base >/dev/null 2>&1 || die "не удалось установить dnsmasq-base"
    fi
    echo "== Скачиваю список доменов российских сервисов"
    fetch_list || true
    build || true
    echo "== Доменов в списке: $(wc -l < "$D/domains.txt")"
    write_units
    systemctl enable --now awgc-dns-update.timer >/dev/null 2>&1
    systemctl enable awgc-dns >/dev/null 2>&1
    systemctl restart awgc-dns
    check_running || { journalctl -u awgc-dns -n 5 --no-pager -o cat; die "DNS-сервер не запустился"; }
    cur=$(current_dns)
    [ -f "$PREV" ] || [ -z "$cur" ] || [ "$cur" = "$ADDR" ] || echo "$cur" > "$PREV"
    echo "== Перевожу DNS пользователей на $ADDR"
    set_clients_dns "$ADDR"
    echo "@@OK"
    ;;
apply)
    build && echo "== Список обновлён"
    if systemctl is-enabled --quiet awgc-dns 2>/dev/null; then
        systemctl restart awgc-dns
        check_running || die "DNS-сервер не запустился"
    fi
    echo "@@DOMAINS $(wc -l < "$D/domains.txt")"
    echo "@@OK"
    ;;
update)
    fetch_list || true
    if build; then systemctl is-active --quiet awgc-dns && systemctl restart awgc-dns; fi
    echo "Доменов в списке: $(wc -l < "$D/domains.txt")"
    ;;
prepare)
    prepare
    ;;
disable)
    prev=$(cat "$PREV" 2>/dev/null); [ -n "$prev" ] || prev="1.1.1.1, 1.0.0.1"
    echo "== Возвращаю пользователям DNS: $prev"
    set_clients_dns "$prev"
    systemctl disable --now awgc-dns-update.timer awgc-dns >/dev/null 2>&1
    rm -f /etc/systemd/system/awgc-dns.service /etc/systemd/system/awgc-dns-update.service /etc/systemd/system/awgc-dns-update.timer
    systemctl daemon-reload
    [ -n "$SUBNET" ] && iptables -t mangle -D PREROUTING -i awg0 -s "$SUBNET" -m set --match-set $SET dst -j RETURN 2>/dev/null
    ipset destroy $SET 2>/dev/null
    for s in $UP_DNS; do ip route del "$s" dev awg1 2>/dev/null; done
    if command -v ufw >/dev/null; then
        while ufw status numbered 2>/dev/null | grep -q awgc-dns; do
            num=$(ufw status numbered | grep awgc-dns | head -1 | sed -E 's/^\[ *([0-9]+)\].*/\1/')
            yes | ufw delete "$num" >/dev/null 2>&1 || break
        done
    fi
    rm -f "$PREV"
    echo "@@OK"
    ;;
status)
    systemctl is-active --quiet awgc-dns && echo "@@ACTIVE 1" || echo "@@ACTIVE 0"
    systemctl is-enabled --quiet awgc-dns 2>/dev/null && echo "@@ENABLED 1" || echo "@@ENABLED 0"
    echo "@@ADDR $ADDR"
    echo "@@DOMAINS $( [ -f "$D/domains.txt" ] && wc -l < "$D/domains.txt" || echo 0)"
    echo "@@IPS $(ipset list $SET 2>/dev/null | awk -F': ' '/Number of entries/{print $2}')"
    echo "@@LISTDATE $( [ -f "$LIST" ] && date -r "$LIST" +%F || true)"
    echo "@@TLDS $( [ -f "$TLDS" ] && echo 1 || echo 0)"
    echo "@@DNS $(current_dns)"
    echo "@@CUSTOM"; [ -f "$CUSTOM" ] && cat "$CUSTOM"; echo "@@END"
    ;;
*)
    echo "использование: $0 enable|apply|disable|update|prepare|status"; exit 2
    ;;
esac
