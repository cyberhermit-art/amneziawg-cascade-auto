#!/bin/bash
# Copyright (c) 2026 AACH
# SPDX-License-Identifier: MIT
#
# awgc-agent.sh — агент на серверах каскада: следит за каскадом сам, пока
# программа на компьютере закрыта, и пишет в Telegram.
#
# Роли (ROLE в awgc-agent.conf):
#   ru    — сервер RU: службы, туннель, проход трафика через выход,
#           сроки действия пользователей, лимиты трафика, репутация адресов,
#           резервная копия в Telegram;
#   watch — сервер EU или выход: следит, что «сосед» (WATCH_PEER — ru_host
#           или mid_host) на связи, и шлёт свою резервную копию. Так о том,
#           что лёг сам сервер RU, сообщит сервер EU.
#
# О проблеме пишет после двух проверок подряд (раз в 5 минут), о решении —
# один раз, когда она ушла.
#
# Команды: run (по таймеру) | test | chatid | backup | status
set -u
A=/root/awg
CONF=$A/awgc-agent.conf
ST=$A/awgc-agent
M=$A/manage_amneziawg.sh
C=/etc/amnezia/amneziawg
mkdir -p "$ST"
[ -f "$CONF" ] || { echo "агент не настроен"; exit 0; }
# shellcheck disable=SC1090
. "$CONF"
ROLE=${ROLE:-ru}; NAME=${NAME:-AmneziaWG}; TG_TOKEN=${TG_TOKEN:-}; TG_CHAT=${TG_CHAT:-}
A_HEALTH=${A_HEALTH:-1}; A_EXPIRY=${A_EXPIRY:-3}; A_LIMITS=${A_LIMITS:-1}; A_REP=${A_REP:-1}
BACKUP=${BACKUP:-off}; HOUR_UTC=${HOUR_UTC:-1}; EXIT_IP=${EXIT_IP:-}; RU_IP=${RU_IP:-}; WATCH_PEER=${WATCH_PEER:-}
NOW=$(date +%s)
# текущий выход (при переключении выходов — не обязательно awg1)
XIF=$(cat $A/awgc-exits/current 2>/dev/null || echo awg1); ip link show "$XIF" >/dev/null 2>&1 || XIF=awg1

# Запрос к api.telegram.org: сначала через туннель (из России Telegram может
# быть недоступен напрямую), потом напрямую.
tgcurl() {
    local m=$1; shift
    local i out
    for i in "$XIF" ""; do
        if [ -n "$i" ]; then ip link show "$i" >/dev/null 2>&1 || continue; fi
        out=$(curl -s -m 40 ${i:+--interface $i} "https://api.telegram.org/bot$TG_TOKEN/$m" "$@" 2>/dev/null) || continue
        case "$out" in *'"ok":true'*) printf '%s' "$out"; return 0 ;; esac
        printf '%s' "$out" > "$ST/last-error"
    done
    return 1
}
tg() { [ -n "$TG_TOKEN" ] && [ -n "$TG_CHAT" ] || return 1
       tgcurl sendMessage --data-urlencode "chat_id=$TG_CHAT" --data-urlencode "text=[$NAME] $1" -d disable_web_page_preview=1 >/dev/null; }
tgfile() { [ -n "$TG_TOKEN" ] && [ -n "$TG_CHAT" ] || return 1
           tgcurl sendDocument -F "chat_id=$TG_CHAT" -F "caption=[$NAME] $2" -F "document=@$1" >/dev/null; }

# Проблема с ключом $1: сообщаем на второй проверке подряд.
problem() {
    local f="$ST/p-$1" n
    n=$(cat "$f" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$f"
    [ "$n" = 2 ] && tg "⚠️ $2"
    return 0
}
resolved() {
    local f="$ST/p-$1" n
    n=$(cat "$f" 2>/dev/null || echo 0)
    [ "$n" -ge 2 ] && tg "✅ $2"
    rm -f "$f"
}
# Однократное событие (ключ не повторяется): истечение срока, лимит за месяц.
once() { [ -f "$ST/o-$1" ] && return 0; tg "$2" && echo "$NOW" > "$ST/o-$1"; }

hs_age() { # возраст рукопожатия интерфейса $1 (первого пира) или пира $2
    local h
    if [ -n "${2:-}" ]; then h=$(awg show "$1" latest-handshakes 2>/dev/null | awk -v k="$2" '$1==k{print $2}')
    else h=$(awg show "$1" latest-handshakes 2>/dev/null | awk 'NR==1{print $2}'); fi
    if [ -n "$h" ] && [ "$h" != 0 ]; then echo $((NOW-h)); else echo -1; fi
}
peer_key() { awk -v n="#_Name = $1" '$0==n{f=1;next} f&&/^PublicKey[ \t]*=/{sub(/^PublicKey[ \t]*=[ \t]*/,"");print;exit}' "$C/awg0.conf" 2>/dev/null; }
hbytes() { awk -v b="$1" 'BEGIN{split("Б КБ МБ ГБ ТБ",u," ");i=1;while(b>=1024&&i<5){b/=1024;i++};printf (i>1?"%.1f %s":"%d %s"),b,u[i]}'; }

check_health_ru() {
    if systemctl is-active --quiet awg-quick@awg0; then resolved awg0 "VPN-сервер на RU снова работает."
    else problem awg0 "VPN-сервер на RU не работает (awg-quick@awg0). Пользователи не могут подключиться."; fi
    [ -f "$C/awg1.conf" ] || return 0
    local age; age=$(hs_age "$XIF")
    if [ "$age" -ge 0 ] && [ "$age" -le 300 ]; then
        resolved tunnel "Туннель от сервера RU к зарубежному серверу снова на связи."
        # Туннель жив — проверяем, что трафик реально выходит в интернет
        # (на трёх серверах это заодно проверка транзит → выход).
        if curl -s -m 15 --interface "$XIF" -o /dev/null -w '%{http_code}' https://www.gstatic.com/generate_204 2>/dev/null | grep -q 204 \
           || curl -s -m 15 --interface "$XIF" -o /dev/null https://1.1.1.1 2>/dev/null; then
            resolved exit "Зарубежный трафик снова проходит."
        else
            problem exit "Туннель к зарубежному серверу на связи, но трафик в интернет через него не проходит. Если серверов три — не отвечает выход (или туннель транзит → выход). Зарубежные сайты сейчас не открываются."
        fi
    else
        problem tunnel "Зарубежный сервер не отвечает в туннеле (последняя связь: $([ "$age" -lt 0 ] && echo никогда || echo "$((age/60)) мин назад")). Зарубежные сайты сейчас не открываются, российские работают."
    fi
    if systemctl is-active --quiet awg-routing 2>/dev/null || ! systemctl cat awg-routing >/dev/null 2>&1; then resolved routing "Раздельная маршрутизация снова работает."
    else problem routing "Раздельная маршрутизация не работает (awg-routing): весь трафик может идти не тем путём."; fi
}

check_watch() {
    [ -n "$WATCH_PEER" ] || return 0
    local k age who
    k=$(peer_key "$WATCH_PEER"); [ -n "$k" ] || return 0
    age=$(hs_age awg0 "$k")
    who="сервер RU"; [ "$WATCH_PEER" = mid_host ] && who="транзит"
    if [ "$age" -ge 0 ] && [ "$age" -le 420 ]; then resolved "peer-$WATCH_PEER" "$who снова на связи."
    else problem "peer-$WATCH_PEER" "$who не выходит на связь ($([ "$age" -lt 0 ] && echo "ни разу с запуска" || echo "уже $((age/60)) мин")). Возможно, сервер выключен, завис или сменил адрес."; fi
}

check_expiry() {
    [ "${A_EXPIRY:-0}" -gt 0 ] 2>/dev/null || return 0
    [ -f "$M" ] || return 0
    bash "$M" --json --no-color list 2>/dev/null | tr -d '\n' | grep -o '{[^{}]*}' | while read -r o; do
        n=$(printf '%s' "$o" | sed -n 's/.*"name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
        e=$(printf '%s' "$o" | sed -n 's/.*"expires_at"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p')
        [ -n "$n" ] && [ -n "$e" ] || continue
        left=$((e-NOW))
        if [ "$left" -le 0 ]; then once "exp0-$n-$e" "⏰ У пользователя $n истёк срок действия ключа — подключение больше не работает."
        elif [ "$left" -le $((A_EXPIRY*86400)) ]; then once "expw-$n-$e" "⏰ У пользователя $n срок действия ключа истекает через $(( (left+3599)/3600 )) ч ($(date -u -d @"$e" '+%d.%m %H:%M' 2>/dev/null) UTC)."; fi
    done
}

check_limits() {
    [ "$A_LIMITS" = 1 ] || return 0
    local L=$A/awgc-limits.txt T=$A/awgc-stats/daily.tsv mon
    [ -s "$L" ] && [ -s "$T" ] || return 0
    mon=$(date +%Y-%m)
    awk -v m="$mon" 'NR==FNR{ if ($1 ~ /^[A-Za-z0-9_-]+$/ && $2+0>0) lim[$1]=$2*1073741824; next }
        substr($1,1,7)==m { u[$2]+=$3+$4 } END { for (n in lim) printf "%s\t%.0f\t%.0f\n", n, u[n]+0, lim[n] }' \
        FS='[ \t]+' "$L" FS='\t' "$T" | while IFS=$'\t' read -r n used lim; do
        if [ "$used" -ge "$lim" ]; then once "lim100-$mon-$n" "📊 Пользователь $n исчерпал месячный лимит трафика: $(hbytes "$used") из $(hbytes "$lim")."
        elif [ "$used" -ge $((lim*8/10)) ]; then once "lim80-$mon-$n" "📊 Пользователь $n израсходовал 80% месячного лимита: $(hbytes "$used") из $(hbytes "$lim")."; fi
    done
}

# Репутация: раз в сутки спрашиваем ip-api.com, не помечен ли адрес как VPN/прокси
# и в какой он стране. Сообщаем только об изменениях.
check_rep() {
    [ "$A_REP" = 1 ] || return 0
    local f=$ST/rep-day; [ "$(cat $f 2>/dev/null)" = "$(date +%F)" ] && return 0
    local ip role j proxy cc prev
    for role in exit entry; do
        if [ $role = exit ]; then ip=$EXIT_IP; else ip=$RU_IP; fi
        [ -n "$ip" ] || continue
        j=$(curl -s -m 20 "http://ip-api.com/json/$ip?fields=status,countryCode,proxy,hosting,isp" 2>/dev/null)
        case "$j" in *'"status":"success"'*) ;; *) continue ;; esac
        proxy=$(printf '%s' "$j" | grep -o '"proxy":[a-z]*' | cut -d: -f2)
        cc=$(printf '%s' "$j" | grep -o '"countryCode":"[A-Z]*"' | cut -d'"' -f4)
        prev=$(cat "$ST/rep-$role" 2>/dev/null)
        echo "$proxy $cc" > "$ST/rep-$role"
        [ -n "$prev" ] && [ "$prev" = "$proxy $cc" ] && continue
        if [ $role = exit ]; then
            [ "$proxy" = true ] && tg "🛡 Адрес выхода $ip теперь помечен в базах как VPN/прокси (ip-api.com). Сервисы с такой проверкой могут его не пускать — подумайте о смене адреса выхода."
            [ "$proxy" = false ] && [ -n "$prev" ] && tg "🛡 Адрес выхода $ip больше не помечен как VPN/прокси (ip-api.com)."
            [ "$cc" = RU ] && tg "🛡 Адрес выхода $ip теперь считается российским (ip-api.com) — сайты увидят Россию."
        else
            [ "$proxy" = true ] && tg "🛡 Адрес сервера RU $ip помечен в базах как VPN/прокси (ip-api.com). Российские сервисы с такой проверкой могут не пускать пользователей."
            [ -n "$cc" ] && [ "$cc" != RU ] && tg "🛡 Адрес сервера RU $ip теперь считается не российским ($cc, ip-api.com) — часть российских сервисов увидит заграницу."
        fi
    done
    date +%F > "$f"
}

do_backup() {
    local P="" f out t
    for f in etc/amnezia/amneziawg root/awg etc/systemd/system/awg-routing.service etc/cron.d/awg-routing-refresh etc/cron.d/awgc-stats; do
        [ -e "/$f" ] && P="$P $f"
    done
    [ -n "$P" ] || return 1
    t=$(mktemp -d); out="$t/$(echo "$NAME" | tr -c 'A-Za-z0-9_.-' '_')-$(hostname -s)-$(date +%Y-%m-%d).tar.gz"
    # shellcheck disable=SC2086
    tar czf "$out" -C / --exclude=root/awg/backups --exclude='root/awg/*.lock' --exclude='root/awg/.*.lock' --exclude=root/awg/awgc-agent.conf $P 2>/dev/null
    tgfile "$out" "Резервная копия сервера $(hostname -s) ($(date '+%d.%m.%Y')). Восстановить: вкладка «Резервные копии» в программе (положите архив в папку копий профиля как ru.tar.gz / eu.tar.gz / ex.tar.gz)."
    local rc=$?; rm -rf "$t"; return $rc
}
check_backup() {
    [ "$BACKUP" = daily ] || [ "$BACKUP" = weekly ] || return 0
    [ "$(date -u +%-H)" = "$HOUR_UTC" ] || return 0
    [ "$BACKUP" = weekly ] && [ "$(date -u +%u)" != 1 ] && return 0
    local f=$ST/backup-day; [ "$(cat $f 2>/dev/null)" = "$(date -u +%F)" ] && return 0
    if do_backup; then date -u +%F > "$f"; fi
}

case "${1:-run}" in
run)
    exec 9>"$ST/.lock"; flock -n 9 || exit 0
    if [ "$ROLE" = ru ]; then
        [ "$A_HEALTH" = 1 ] && check_health_ru
        check_expiry; check_limits; check_rep
    else
        [ "$A_HEALTH" = 1 ] && check_watch
    fi
    check_backup
    date +%s > "$ST/last-run"
    ;;
test)
    if tg "🔔 Проверка связи: агент на сервере $(hostname -s) ($([ "$ROLE" = ru ] && echo "RU" || echo "$WATCH_PEER → наблюдение")) работает."; then echo "@@OK"; else echo "@@FAIL $(head -c 300 "$ST/last-error" 2>/dev/null)"; fi
    ;;
chatid)
    out=$(tgcurl getUpdates) || { echo "@@FAIL $(head -c 300 "$ST/last-error" 2>/dev/null)"; exit 0; }
    id=$(printf '%s' "$out" | grep -o '"chat":{"id":-\{0,1\}[0-9]*' | tail -1 | grep -o -- '-\{0,1\}[0-9]*$')
    [ -n "$id" ] && echo "@@CHAT $id" || echo "@@NONE"
    ;;
backup)
    if do_backup; then echo "@@OK"; else echo "@@FAIL $(head -c 300 "$ST/last-error" 2>/dev/null)"; fi
    ;;
status)
    echo "@@ACTIVE $(systemctl is-active awgc-agent.timer 2>/dev/null)"
    echo "@@LAST $(cat "$ST/last-run" 2>/dev/null)"
    echo "@@NOW $NOW"
    ;;
*) echo "использование: $0 run|test|chatid|backup|status"; exit 2 ;;
esac
