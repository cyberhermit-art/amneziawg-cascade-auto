#!/bin/bash
# Copyright (c) 2026 AACH
# SPDX-License-Identifier: MIT
#
# awgc-exits.sh — несколько выходов и автоматическое переключение между ними.
# Работает на «узле выбора выхода»: в каскаде из двух серверов это сервер RU,
# в каскаде из трёх — транзит. Зарубежный трафик идёт по таблице 100, где
# default указывает на туннель текущего выхода; этот скрипт раз в 30 секунд
# проверяет выходы и при отказе переключает default на следующий живой.
#
# Список выходов по приоритету — /root/awg/awgc-exits.conf, строка: «туннель метка»
#   awg1 основной
#   awg2 резерв-1
#   warp WARP
# Возврат на более приоритетный выход — после 3 удачных проверок подряд.
# Если не работает ни один выход, маршрут остаётся на текущем (мёртвом)
# туннеле: трафик блокируется, а не уходит с адреса этого сервера.
#
# Команды: run | status | add ИМЯ ФАЙЛ | remove ИМЯ | warp-on | warp-off | install | uninstall
set -u
A=/root/awg
C=/etc/amnezia/amneziawg
LIST=$A/awgc-exits.conf
ST=$A/awgc-exits
TABLE=100
mkdir -p "$ST"
[ -f "$LIST" ] || printf 'awg1 основной\n' > "$LIST"

# На сервере RU (каскад из двух) свою подсеть клиентов NAT-им только её; на транзите — всё.
SUBNET=$(grep -oP '^CLIENT_SUBNET="\K[^"]+' $A/awg-routing.sh 2>/dev/null | head -1)
nat_args() { if [ -n "$SUBNET" ]; then echo "-s $SUBNET -o $1"; else echo "-o $1"; fi; }

cur() { local c; c=$(cat "$ST/current" 2>/dev/null); [ -n "$c" ] && echo "$c" || awk 'NR==1{print $1}' "$LIST"; }
label() { awk -v i="$1" '$1==i{ $1=""; sub(/^ /,""); print; exit }' "$LIST"; }

probe() { # живой ли выход через туннель $1
    ip link show "$1" >/dev/null 2>&1 || return 1
    curl -s -m 8 --interface "$1" -o /dev/null -w '%{http_code}' https://www.gstatic.com/generate_204 2>/dev/null | grep -q 204 && return 0
    curl -s -m 8 --interface "$1" -o /dev/null https://1.1.1.1 2>/dev/null
}

tg() { # сообщение через агент (если он настроен)
    [ -f $A/awgc-agent.conf ] || return 0
    local TG_TOKEN="" TG_CHAT="" NAME="AmneziaWG" i
    # shellcheck disable=SC1090
    . $A/awgc-agent.conf
    [ -n "$TG_TOKEN" ] && [ -n "$TG_CHAT" ] || return 0
    for i in "$(cur)" ""; do
        if [ -n "$i" ]; then ip link show "$i" >/dev/null 2>&1 || continue; fi
        curl -s -m 30 ${i:+--interface $i} "https://api.telegram.org/bot$TG_TOKEN/sendMessage" \
            --data-urlencode "chat_id=$TG_CHAT" --data-urlencode "text=[$NAME] $1" 2>/dev/null | grep -q '"ok":true' && return 0
    done
}

switch_to() {
    local new=$1 old; old=$(cur)
    ip route replace default dev "$new" table $TABLE || return 1
    echo "$new" > "$ST/current"
    echo "$(date '+%F %T') $old -> $new" >> "$ST/switches.log"
    if [ "$new" = "$(awk 'NR==1{print $1}' "$LIST")" ]; then
        tg "🔁 Зарубежный трафик снова идёт через основной выход ($new)."
    else
        tg "🔁 Выход «$(label "$old")» ($old) не отвечает — зарубежный трафик переключён на «$(label "$new")» ($new)."
    fi
}

write_conf() { # $1 — имя туннеля, $2 — конфиг клиента выхода
    local i=$1 src=$2 na
    na=$(nat_args "$i" | sed 's#/#\\/#g')   # для sed: / в адресе подсети
    grep -viE '^[[:space:]]*(DNS|Table|PostUp|PostDown)[[:space:]]*=' "$src" \
        | sed "0,/^\[Interface\]/s//[Interface]\nTable = off\nPostUp = iptables -t nat -C POSTROUTING $na -j MASQUERADE 2>\/dev\/null || iptables -t nat -A POSTROUTING $na -j MASQUERADE; iptables -t mangle -C FORWARD -o $i -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu 2>\/dev\/null || iptables -t mangle -A FORWARD -o $i -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu\nPostDown = iptables -t nat -D POSTROUTING $na -j MASQUERADE; iptables -t mangle -D FORWARD -o $i -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu/" \
        > "$C/$i.conf.new"
    chmod 600 "$C/$i.conf.new"; mv -f "$C/$i.conf.new" "$C/$i.conf"
}

add_to_list() { grep -q "^$1 " "$LIST" || echo "$1 $2" >> "$LIST"; }
del_from_list() { sed -i "/^$1 /d" "$LIST"; }

install_units() {
    cat > /etc/systemd/system/awgc-exits.service <<'EOF'
[Unit]
Description=AmneziaWG Cascade: exit failover check

[Service]
Type=oneshot
ExecStart=/bin/bash /root/awg/awgc-exits.sh run
EOF
    cat > /etc/systemd/system/awgc-exits.timer <<'EOF'
[Unit]
Description=AmneziaWG Cascade: exit failover every 30 s

[Timer]
OnBootSec=90s
OnUnitActiveSec=30s
AccuracySec=5s

[Install]
WantedBy=timers.target
EOF
    systemctl daemon-reload
    systemctl enable --now awgc-exits.timer >/dev/null 2>&1
}

case "${1:-run}" in
run)
    exec 9>"$ST/.lock"; flock -n 9 || exit 0
    [ "$(wc -l < "$LIST")" -ge 2 ] || exit 0   # один выход — переключать некуда
    c=$(cur); best=""
    while read -r i _; do
        [ -n "$i" ] || continue
        if probe "$i"; then
            n=$(( $(cat "$ST/ok-$i" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$ST/ok-$i"; rm -f "$ST/fail-$i"
            [ -z "$best" ] && best=$i
        else
            n=$(( $(cat "$ST/fail-$i" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$ST/fail-$i"; rm -f "$ST/ok-$i"
        fi
    done < "$LIST"
    date +%s > "$ST/last-run"
    [ -n "$best" ] || exit 0
    [ "$best" = "$c" ] && { ip route show table $TABLE | grep -q "default dev $c" || ip route replace default dev "$c" table $TABLE; exit 0; }
    cfail=$(cat "$ST/fail-$c" 2>/dev/null || echo 0)
    bok=$(cat "$ST/ok-$best" 2>/dev/null || echo 0)
    # текущий упал (2 проверки подряд) — сразу на лучший живой;
    # текущий жив, но ожил более приоритетный — возвращаемся после 3 удачных проверок
    if [ "$cfail" -ge 2 ] || { [ "$cfail" = 0 ] && [ "$bok" -ge 3 ]; }; then switch_to "$best"; fi
    ;;
status)
    c=$(cur)
    echo "@@CUR $c"
    echo "@@LAST $(cat "$ST/last-run" 2>/dev/null)"
    echo "@@NOW $(date +%s)"
    echo "@@TIMER $(systemctl is-active awgc-exits.timer 2>/dev/null)"
    while read -r i l; do
        [ -n "$i" ] || continue
        h=$(awg show "$i" latest-handshakes 2>/dev/null | awk 'NR==1{print $2}')
        ep=$(sed -n -E 's/^Endpoint[[:space:]]*=[[:space:]]*//p' "$C/$i.conf" 2>/dev/null | head -1)
        up=0; ip link show "$i" >/dev/null 2>&1 && up=1
        ok=$(cat "$ST/ok-$i" 2>/dev/null || echo 0); fl=$(cat "$ST/fail-$i" 2>/dev/null || echo 0)
        echo "@@EXIT $i|$l|${h:-0}|$ep|$up|$ok|$fl"
    done < "$LIST"
    tail -5 "$ST/switches.log" 2>/dev/null | sed 's/^/@@SW /'
    ;;
add)  # add ИМЯ_ТУННЕЛЯ ФАЙЛ_КЛИЕНТА [метка]
    i=${2:?}; src=${3:?}; lab=${4:-резерв}
    [ -f "$src" ] || { echo "ОШИБКА: нет файла $src"; exit 1; }
    sed -i 's/\r$//' "$src"
    write_conf "$i" "$src"
    systemctl enable awg-quick@"$i" >/dev/null 2>&1
    systemctl restart awg-quick@"$i" || { echo "ОШИБКА: туннель $i не запустился: $(journalctl -u awg-quick@$i -n 5 --no-pager -o cat | tr '\n' ' ')"; exit 1; }
    add_to_list "$i" "$lab"
    install_units
    ok=0; for _ in 1 2 3 4 5 6; do probe "$i" && { ok=1; break; }; sleep 3; done
    [ $ok = 1 ] && echo "@@OK" || echo "@@NOPROBE"
    ;;
remove)
    i=${2:?}
    [ "$i" = awg1 ] && { echo "ОШИБКА: основной выход так не удаляется"; exit 1; }
    if [ "$(cur)" = "$i" ]; then switch_to "$(awk 'NR==1{print $1}' "$LIST")" || true; fi
    systemctl disable --now awg-quick@"$i" >/dev/null 2>&1
    rm -f "$C/$i.conf" "$ST/ok-$i" "$ST/fail-$i"
    del_from_list "$i"
    if [ "$(wc -l < "$LIST")" -lt 2 ]; then systemctl disable --now awgc-exits.timer >/dev/null 2>&1; fi
    echo "@@OK"
    ;;
warp-on)
    command -v curl >/dev/null || { echo "ОШИБКА: нет curl"; exit 1; }
    priv=$(awg genkey) && pub=$(printf '%s' "$priv" | awg pubkey) || { echo "ОШИБКА: не удалось создать ключ"; exit 1; }
    r=$(curl -s -m 30 -X POST -H 'User-Agent: okhttp/3.12.1' -H 'CF-Client-Version: a-6.10-2158' -H 'Content-Type: application/json' \
        -d "{\"key\":\"$pub\",\"install_id\":\"\",\"fcm_token\":\"\",\"tos\":\"$(date -u +%Y-%m-%dT%H:%M:%S.000Z)\",\"model\":\"PC\",\"serial_number\":\"\",\"locale\":\"en_US\"}" \
        https://api.cloudflareclient.com/v0a2158/reg)
    ppub=$(printf '%s' "$r" | grep -o '"public_key":"[^"]*"' | head -1 | cut -d'"' -f4)
    v4=$(printf '%s' "$r" | grep -o '"v4":"172\.[0-9.]*"' | head -1 | cut -d'"' -f4)
    [ -n "$ppub" ] && [ -n "$v4" ] || { echo "ОШИБКА: Cloudflare не выдал настройки WARP: $(printf '%s' "$r" | head -c 200)"; exit 1; }
    cat > "$ST/warp-src.conf" <<EOF
[Interface]
PrivateKey = $priv
Address = $v4/32

[Peer]
PublicKey = $ppub
AllowedIPs = 0.0.0.0/0
Endpoint = engage.cloudflareclient.com:2408
PersistentKeepalive = 25
EOF
    chmod 600 "$ST/warp-src.conf"
    write_conf warp "$ST/warp-src.conf"
    sed -i 's/^Table = off$/Table = off\nMTU = 1280/' "$C/warp.conf"
    systemctl enable awg-quick@warp >/dev/null 2>&1
    systemctl restart awg-quick@warp || { echo "ОШИБКА: туннель WARP не запустился: $(journalctl -u awg-quick@warp -n 5 --no-pager -o cat | tr '\n' ' ')"; exit 1; }
    add_to_list warp WARP
    install_units
    ok=0; for _ in 1 2 3 4 5 6; do probe warp && { ok=1; break; }; sleep 3; done
    [ $ok = 1 ] && echo "@@OK" || echo "@@NOPROBE"
    ;;
warp-off)
    exec bash "$0" remove warp
    ;;
uninstall)
    for i in $(awk 'NR>1{print $1}' "$LIST"); do systemctl disable --now awg-quick@"$i" >/dev/null 2>&1; rm -f "$C/$i.conf"; done
    systemctl disable --now awgc-exits.timer awgc-exits.service >/dev/null 2>&1
    rm -f /etc/systemd/system/awgc-exits.service /etc/systemd/system/awgc-exits.timer; systemctl daemon-reload
    rm -rf "$ST" "$LIST"
    echo "@@OK"
    ;;
*) echo "использование: $0 run|status|add|remove|warp-on|warp-off|uninstall"; exit 2 ;;
esac
