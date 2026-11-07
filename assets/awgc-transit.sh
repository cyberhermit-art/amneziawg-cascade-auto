#!/bin/bash
# Copyright (c) 2026 AACH
# SPDX-License-Identifier: MIT
#
# awgc-transit.sh — средний (транзитный) сервер трёхзвенного каскада:
#   клиент -> RU -> ТРАНЗИТ -> ВЫХОД -> сайт
# Транзит ничего не делит: всё, что пришло от сервера RU (через awg0), он
# отправляет в туннель awg1 к серверу-выходу. Адрес транзита наружу не виден,
# поэтому если адрес выхода попадёт в чёрный список, сервер RU и пользователей
# это не затронет — меняется только выход.
#
# Рецепт — от автора amneziawg-installer (обсуждение трёхзвенного каскада):
# awg1.conf из клиента выхода с Table = off, без DNS, плюс правило
# «всё, что пришло через awg0, — в таблицу 100, а там default через awg1».
# Собственный трафик сервера (включая сам туннель к выходу) идёт по основной
# таблице, поэтому петли нет.
#
# Команды:
#   setup  ФАЙЛ  — создать туннель к выходу из конфига клиента (mid_host.conf)
#   remove       — убрать туннель
#   status       — состояние для программы
set -u
C=/etc/amnezia/amneziawg
CONF=$C/awg1.conf
PRIO=10000
TABLE=100

say() { echo "==> $*"; }
die() { echo "ОШИБКА: $*"; exit 1; }

hs_age() {
    local hs now
    hs=$(awg show awg1 latest-handshakes 2>/dev/null | awk 'NR==1{print $2}')
    now=$(date +%s)
    if [ -n "$hs" ] && [ "$hs" != 0 ]; then echo $((now-hs)); else echo -1; fi
}

case "${1:-}" in
setup)
    SRC=${2:-}
    [ -f "$SRC" ] || die "нет файла конфигурации выхода ($SRC)"
    [ -f $C/awg0.conf ] || die "на этом сервере не установлен AmneziaWG"
    sed -i 's/\r$//' "$SRC"
    grep -q '^\[Peer\]' "$SRC" || die "полученный файл не похож на конфиг AmneziaWG"
    say "Создаю туннель awg1 к серверу-выходу"
    # прежний туннель (если был) — остановить, чтобы правила PostDown убрались
    systemctl stop awg-quick@awg1 2>/dev/null || true
    grep -viE '^[[:space:]]*(DNS|Table|PostUp|PostDown)[[:space:]]*=' "$SRC" \
        | sed "0,/^\[Interface\]/s//[Interface]\nTable = off\nPostUp = ip rule add iif awg0 lookup $TABLE priority $PRIO; ip route replace default dev awg1 table $TABLE; iptables -t nat -A POSTROUTING -o awg1 -j MASQUERADE\nPostDown = ip rule del iif awg0 lookup $TABLE priority $PRIO; ip route flush table $TABLE; iptables -t nat -D POSTROUTING -o awg1 -j MASQUERADE/" \
        > "$CONF.new"
    chmod 600 "$CONF.new"
    mv -f "$CONF.new" "$CONF"
    systemctl enable awg-quick@awg1 >/dev/null 2>&1
    systemctl restart awg-quick@awg1 || die "awg-quick@awg1 не запустился: $(journalctl -u awg-quick@awg1 -n 5 --no-pager -o cat 2>/dev/null | tr '\n' ' ')"
    echo -n "Жду рукопожатия с сервером-выходом"
    ok=0
    for _ in $(seq 1 20); do
        a=$(hs_age)
        if [ "$a" -ge 0 ] && [ "$a" -lt 120 ]; then ok=1; break; fi
        ping -c1 -W1 -I awg1 1.1.1.1 >/dev/null 2>&1
        echo -n "."; sleep 1
    done
    echo
    [ "$ok" = 1 ] || die "туннель к выходу не установил связь — проверьте, что на выходе работает AmneziaWG и открыт его порт ($(sed -n -E 's/^Endpoint[[:space:]]*=[[:space:]]*.*:([0-9]+)$/\1/p' "$CONF" | head -1)/udp)"
    echo "Связь с сервером-выходом есть."
    say "Проверка"
    chk() { if eval "$2" >/dev/null 2>&1; then echo "  [OK]   $1"; else echo "  [FAIL] $1"; fails=$((fails+1)); fi; }
    fails=0
    chk "правило: трафик от сервера RU — в таблицу $TABLE" "ip rule | grep -q 'iif awg0 lookup $TABLE'"
    chk "в таблице $TABLE путь через awg1"               "ip route show table $TABLE | grep -q 'dev awg1'"
    chk "NAT на awg1"                                   "iptables -t nat -C POSTROUTING -o awg1 -j MASQUERADE"
    chk "автозапуск туннеля"                            "systemctl is-enabled awg-quick@awg1"
    [ "$fails" = 0 ] || die "транзит настроен не полностью ($fails проверок не прошло)"
    say "Транзит работает: всё, что приходит от сервера RU, уходит через сервер-выход."
    ;;
remove)
    systemctl disable --now awg-quick@awg1 >/dev/null 2>&1 || true
    rm -f "$CONF"
    # на случай, если PostDown не отработал
    while ip rule del iif awg0 lookup $TABLE priority $PRIO 2>/dev/null; do :; done
    ip route flush table $TABLE 2>/dev/null || true
    while iptables -t nat -D POSTROUTING -o awg1 -j MASQUERADE 2>/dev/null; do :; done
    echo "@@OK"
    ;;
status)
    echo "@@CONF $([ -f "$CONF" ] && echo 1 || echo 0)"
    echo "@@ACTIVE $(systemctl is-active awg-quick@awg1 2>/dev/null)"
    echo "@@AGE $(hs_age)"
    echo "@@RULE $(ip rule 2>/dev/null | grep -q "iif awg0 lookup $TABLE" && echo 1 || echo 0)"
    echo "@@EP $(sed -n -E 's/^Endpoint[[:space:]]*=[[:space:]]*//p' "$CONF" 2>/dev/null | head -1)"
    ;;
*)
    echo "использование: $0 setup ФАЙЛ | remove | status"; exit 2
    ;;
esac
