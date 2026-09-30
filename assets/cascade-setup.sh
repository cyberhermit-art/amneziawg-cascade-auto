#!/bin/bash
# Copyright (c) 2026 AACH
# SPDX-License-Identifier: MIT
#
# Встроенный ниже скрипт маршрутизации (между маркерами ROUTING_EOF) взят без изменений
# из CASCADE.md проекта https://github.com/bivlked/amneziawg-installer
# Copyright (c) 2025-2026 Ivan Bondarev, лицензия MIT (см. THIRD_PARTY_NOTICES.md).
# cascade-setup.sh — настройка каскада AmneziaWG одной командой на ВХОДНОМ сервере (RU).
# Делает шаги 3–5 из CASCADE.md (bivlked/amneziawg-installer):
#   туннель awg1 к серверу EU, скрипт маршрутизации, автозапуск, еженедельное обновление списка.
#
# Использование (на сервере RU, от root):
#   bash cascade-setup.sh root@IP_СЕРВЕРА_EU     # сам создаст клиента на EU по SSH и заберёт конфиг
#   bash cascade-setup.sh /root/ru_host.conf     # если конфиг клиента уже скопирован вручную
#   bash cascade-setup.sh --remove               # убрать каскад с этого сервера
#
# Повторный запуск безопасен: всё перезаписывается заново.
set -euo pipefail

AWG_DIR="/root/awg"
CONF_DIR="/etc/amnezia/amneziawg"
AWG0_CONF="$CONF_DIR/awg0.conf"
AWG1_CONF="$CONF_DIR/awg1.conf"
ROUTING="$AWG_DIR/awg-routing.sh"
UNIT="/etc/systemd/system/awg-routing.service"
CRON="/etc/cron.d/awg-routing-refresh"
CLIENT_NAME="ru_host"

say()  { printf '\n\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mВНИМАНИЕ:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mОШИБКА:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "запустите от root"

# ---------- IPv4-арифметика для проверки подсетей ----------
ip2int() { local IFS=.; read -r a b c d <<<"$1"; echo $(( (a<<24) + (b<<16) + (c<<8) + d )); }
int2ip() { local n=$1; echo "$(( (n>>24)&255 )).$(( (n>>16)&255 )).$(( (n>>8)&255 )).$(( n&255 ))"; }
net_of() {  # 172.16.17.1/24 -> 172.16.17.0/24
    local ip=${1%/*} len=${1#*/} mask
    mask=$(( len == 0 ? 0 : (0xFFFFFFFF << (32 - len)) & 0xFFFFFFFF ))
    echo "$(int2ip $(( $(ip2int "$ip") & mask )))/$len"
}
in_net() {  # in_net 172.16.61.4 172.16.17.0/24
    local net=${2%/*} len=${2#*/} mask
    mask=$(( len == 0 ? 0 : (0xFFFFFFFF << (32 - len)) & 0xFFFFFFFF ))
    [ $(( $(ip2int "$1") & mask )) -eq $(( $(ip2int "$net") & mask )) ]
}
conf_val() {  # conf_val <файл> <ключ> — первое значение ключа из конфига
    { grep -iE "^[[:space:]]*$2[[:space:]]*=" "$1" || true; } | head -1 | cut -d= -f2- | tr -d '[:space:]'
}

# ---------- удаление каскада ----------
if [ "${1:-}" = "--remove" ]; then
    say "Убираю каскад"
    SUBNET="172.16.17.0/24"; EP=""
    if [ -f "$ROUTING" ]; then
        SUBNET=$(grep -oP '^CLIENT_SUBNET="\K[^"]+' "$ROUTING" || echo "$SUBNET")
        EP=$(grep -oP '^AWG1_ENDPOINT="\K[^"]+' "$ROUTING" || true)
    fi
    systemctl disable --now awg-routing.timer 2>/dev/null || true
    systemctl disable --now awg-routing awg-quick@awg1 2>/dev/null || true
    rm -f "$UNIT" /etc/systemd/system/awg-routing.timer "$CRON"
    systemctl daemon-reload
    iptables -t mangle -D PREROUTING -i awg0 -s "$SUBNET" -m set --match-set ru dst -j RETURN 2>/dev/null || true
    iptables -t mangle -D PREROUTING -i awg0 -s "$SUBNET" -j MARK --set-mark 0x1 2>/dev/null || true
    iptables -t nat -D POSTROUTING -s "$SUBNET" -o awg1 -j MASQUERADE 2>/dev/null || true
    ip rule del fwmark 0x1 table 100 2>/dev/null || true
    ip route del default dev awg1 table 100 2>/dev/null || true
    [ -n "$EP" ] && ip route del "$EP" 2>/dev/null || true
    ipset destroy ru_tmp 2>/dev/null || true
    ipset destroy ru 2>/dev/null || true
    rm -f "$AWG1_CONF" "$ROUTING" "$AWG_DIR/ru.zone" "$AWG_DIR/ru.zone.tmp" "$AWG_DIR/awg-routing.lock"
    say "Готово. На сервере EU удалите клиента: bash /root/awg/manage_amneziawg.sh remove $CLIENT_NAME"
    exit 0
fi

SRC="${1:-}"
[ -n "$SRC" ] || die "укажите root@IP_СЕРВЕРА_EU или путь к ru_host.conf (см. начало скрипта)"

# ---------- 0. Проверки ----------
say "Проверяю сервер"
[ -f "$AWG0_CONF" ] || die "нет $AWG0_CONF — сначала установите AmneziaWG на этот сервер (шаг 1)"
command -v awg-quick >/dev/null || die "не найден awg-quick — установка AmneziaWG не завершена"

if ! command -v curl >/dev/null || ! command -v ipset >/dev/null; then
    say "Ставлю curl и ipset"
    apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl ipset >/dev/null
fi

AWG0_ADDR=$(conf_val "$AWG0_CONF" Address | cut -d, -f1)
[[ "$AWG0_ADDR" =~ ^[0-9.]+/[0-9]+$ ]] || die "не смог прочитать Address из $AWG0_CONF"
CLIENT_SUBNET=$(net_of "$AWG0_ADDR")
echo "Подсеть клиентов этого сервера: $CLIENT_SUBNET"

# ---------- 1. Конфиг клиента от сервера EU ----------
TMP_CONF=$(mktemp)
trap 'rm -f "$TMP_CONF"' EXIT
if [ -f "$SRC" ]; then
    cp "$SRC" "$TMP_CONF"
else
    say "Подключаюсь к $SRC по SSH (может спросить пароль от сервера EU)"
    # Одно SSH-подключение: при необходимости создаёт клиента и выводит его конфиг.
    ssh -o StrictHostKeyChecking=accept-new "$SRC" \
        "[ -f $AWG_DIR/$CLIENT_NAME.conf ] || bash $AWG_DIR/manage_amneziawg.sh add $CLIENT_NAME >/dev/null; cat $AWG_DIR/$CLIENT_NAME.conf" \
        > "$TMP_CONF" || die "не удалось получить конфиг с $SRC. Проверьте SSH-доступ и что AmneziaWG там установлен"
fi
sed -i 's/\r$//' "$TMP_CONF"   # на случай копирования из Windows
grep -q '^\[Peer\]' "$TMP_CONF" || die "полученный файл не похож на конфиг AmneziaWG"

ENDPOINT=$(conf_val "$TMP_CONF" Endpoint)
EP_HOST=${ENDPOINT%:*}
if [[ ! "$EP_HOST" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    EP_HOST=$(getent ahostsv4 "$EP_HOST" | awk 'NR==1{print $1}' || true)
fi
[[ "$EP_HOST" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "не смог определить IP сервера EU из Endpoint ($ENDPOINT)"
echo "Сервер EU: $EP_HOST"

AWG1_ADDR=$(conf_val "$TMP_CONF" Address | cut -d, -f1)
if in_net "${AWG1_ADDR%/*}" "$CLIENT_SUBNET"; then
    die "подсети серверов совпадают ($AWG1_ADDR внутри $CLIENT_SUBNET). Переустановите один из серверов с другим --subnet"
fi

# ---------- 2. Туннель awg1 ----------
say "Создаю туннель awg1 к серверу EU"
# Убираем DNS и старый Table, добавляем Table = off сразу после [Interface]
grep -viE '^[[:space:]]*(DNS|Table)[[:space:]]*=' "$TMP_CONF" \
    | sed '0,/^\[Interface\]/s//[Interface]\nTable = off/' > "$AWG1_CONF"
chmod 600 "$AWG1_CONF"
systemctl restart awg-quick@awg1

echo -n "Жду рукопожатия с сервером EU"
ok=0
for _ in $(seq 1 20); do
    hs=$(awg show awg1 latest-handshakes 2>/dev/null | awk 'NR==1{print $2}' || true)
    if [ -n "$hs" ] && [ "$hs" != "0" ]; then ok=1; break; fi
    echo -n "."; sleep 1
done
echo
[ "$ok" = 1 ] || die "туннель awg1 не установил связь. Проверьте, что на EU работает AmneziaWG и открыт его порт (${ENDPOINT##*:}/udp)"
echo "Связь с сервером EU есть."

# ---------- 3. Скрипт маршрутизации (оригинал из CASCADE.md) ----------
say "Записываю $ROUTING"
mkdir -p "$AWG_DIR"
cat > "$ROUTING" <<'ROUTING_EOF'
#!/bin/bash
# awg-routing.sh - каскадный сплит-роутинг на сервере AWG0 (вход).
# RU-трафик клиентов идёт напрямую через WAN, остальное - через туннель awg1 на сервер-выход AWG1.
# Скрипт идемпотентен: его можно запускать повторно (в т.ч. по расписанию для обновления RU-сетей).
set -euo pipefail

# ===== параметры (поправь под свою установку) =====
CLIENT_SUBNET="172.16.17.0/24"          # подсеть клиентов AWG0 (см. Address в /etc/amnezia/amneziawg/awg0.conf)
AWG1_IF="awg1"                           # имя интерфейса туннеля к серверу-выходу
AWG1_ENDPOINT="CHANGE_ME"               # внешний IP сервера AWG1 (Endpoint из awg1.conf, без порта)
TABLE_ID=100                            # номер таблицы маршрутизации для трафика "на выход"
FWMARK="0x1"                            # метка для трафика, уходящего через awg1
RULE_PRIO=10000                        # приоритет правила ip rule (нестандартный, чтобы не конфликтовать)
RU_ZONE_URL="https://www.ipdeny.com/ipblocks/data/aggregated/ru-aggregated.zone"
RU_ZONE_FALLBACK_URL="https://raw.githubusercontent.com/bivlked/amneziawg-installer/v5.37.0/cascade/ru.zone"
AWG_DIR="/root/awg"
EXTRA_RU_NETS=""                        # свои адреса/сети, которые вести через РФ (через пробел),
                                        # напр. "203.0.113.7 198.51.100.0/24". Нужно для .ru-сайтов
                                        # за зарубежным CDN: по адресу они выглядят как заграница
# ==================================================

RU_ZONE="$AWG_DIR/ru.zone"
mkdir -p "$AWG_DIR"

# Блокировка: два одновременных запуска мешают друг другу. Оба наперегонки подменяют один и тот же
# набор ru через ru_tmp, и проигравший падает с "The set with the given name does not exist"; вдобавок
# оба пишут в общий "$RU_ZONE.tmp", а trap первого удаляет файл второго. Типичный случай - юнит
# awg-routing стартует после перезагрузки, а вы тут же запускаете скрипт руками (issue #212).
# Здесь второй экземпляр ЖДЁТ, а не ломается.
exec 9>"$AWG_DIR/awg-routing.lock"
flock -w 300 9 || { echo "ERROR: другой экземпляр awg-routing.sh не завершился за 5 минут - прерываюсь" >&2; exit 1; }

trap 'rm -f "$RU_ZONE.tmp"' EXIT         # не оставлять временный файл списка при выходе/прерывании

[ "$AWG1_ENDPOINT" != "CHANGE_ME" ] || { echo "ERROR: впиши AWG1_ENDPOINT (внешний IP сервера AWG1)" >&2; exit 1; }

# Определяем выход к AWG1 по самому маршруту до endpoint (надёжнее парсинга default - работает и на
# on-link/point-to-point/multi-homed). На первом запуске это путь через WAN (туннель ещё не вмешивается).
EP_ROUTE="$(ip -4 route get "$AWG1_ENDPOINT" 2>/dev/null | head -1)"
WAN_IF="$(printf '%s' "$EP_ROUTE" | grep -oP '\bdev \K\S+' || true)"
WAN_GW="$(printf '%s' "$EP_ROUTE" | grep -oP '\bvia \K\S+' || true)"
{ [ -n "$WAN_IF" ] && [ "$WAN_IF" != "$AWG1_IF" ]; } \
    || { echo "ERROR: не удалось определить WAN-интерфейс к AWG1 (или он указывает в туннель)" >&2; exit 1; }

# Предупреждение про IPv6: схема только для IPv4, при включённом IPv6 он пойдёт мимо деления.
if [ "$(cat /proc/sys/net/ipv6/conf/all/disable_ipv6 2>/dev/null || echo 1)" = "0" ] \
   && ip -6 route show default 2>/dev/null | grep -q .; then
    echo "WARN: на сервере включён IPv6 - IPv6-трафик пойдёт мимо каскада. Отключи IPv6 (установщик: --disallow-ipv6)." >&2
fi

# 1) Обновить список RU-сетей. Источники по порядку: ipdeny (актуальный) -> снимок в репозитории
#    (если ipdeny недоступен) -> уже лежащий локальный список. Рабочий файл заменяем только при
#    успешной непустой загрузке, поэтому сорванная закачка не обнулит прежний список.
fetch_ru_zone() {                                # $1 = URL; качает и ПРОВЕРЯЕТ содержимое, 0 при успехе
    # Таймауты обязательны: скрипт запускается юнитом при загрузке, а у юнита ограничено время.
    # Без них зависшая закачка съела бы весь бюджет, и systemd убил бы юнит до применения правил.
    # --retry-max-time ограничивает СУММУ попыток, иначе три повтора по 30 с дают 93 с на один URL.
    curl -fsS --retry 2 --retry-max-time 45 --connect-timeout 10 --max-time 30 \
        -o "$RU_ZONE.tmp" "$1" || return 1
    # 🔴 Проверять надо СОДЕРЖИМОЕ, а не только непустоту. curl -f отсекает только код >= 400,
    # поэтому заглушка провайдера, страница DPI или капча, отданные с кодом 200, прошли бы как
    # валидный список, затёрли рабочий файл и сломали деление до ручного вмешательства.
    local valid total
    valid=$(grep -cE '^[0-9]{1,3}(\.[0-9]{1,3}){3}/[0-9]{1,2}$' "$RU_ZONE.tmp" || true)
    # Считаем НЕПУСТЫЕ строки, а не все: иначе отсутствие завершающего перевода строки или
    # пустая строка в конце отвергали бы годный список, хотя загрузчик ниже их спокойно терпит.
    total=$(grep -c '[^[:space:]]' "$RU_ZONE.tmp" || true)
    if [ "$valid" -lt 1000 ] || [ "$valid" -ne "$total" ]; then
        echo "WARN: $1 отдал не список RU-сетей (валидных строк $valid из $total) - не беру" >&2
        return 1
    fi
}
if fetch_ru_zone "$RU_ZONE_URL"; then
    mv -f "$RU_ZONE.tmp" "$RU_ZONE"
elif fetch_ru_zone "$RU_ZONE_FALLBACK_URL"; then
    mv -f "$RU_ZONE.tmp" "$RU_ZONE"
    echo "WARN: ipdeny недоступен - взял снимок RU-сетей из репозитория (может немного отставать от актуального)" >&2
else
    echo "WARN: не удалось скачать список ни с ipdeny, ни из репозитория - использую прежний локальный" >&2
fi
# Без списка не продолжаем: пустой ipset отправит ВЕСЬ трафик за границу (деление молча сломается).
[ -s "$RU_ZONE" ] || { echo "ERROR: список RU-сетей пуст и нигде не найден - прерываюсь, чтобы не сломать деление" >&2; exit 1; }

# 2) Загрузить RU-сети в ipset через временный сет (атомарная замена, без "пустого окна").
ipset create ru hash:net -exist
ipset create ru_tmp hash:net -exist
ipset flush ru_tmp
while read -r net; do
    [ -n "$net" ] && ipset add ru_tmp "$net" -exist
done < "$RU_ZONE"
# Свои адреса добавляем в тот же временный набор ДО подмены - тогда они переживают запуск скрипта.
# Дописывать в готовый ru бесполезно: следующий запуск соберёт набор заново и затрёт добавленное.
# set -f отключает раскрытие шаблонов: без него звёздочка в переменной превратилась бы в имена файлов.
# Ошибочная запись НЕ роняет скрипт: при запуске из юнита это оставило бы сервер без деления вовсе,
# а так теряется только само исключение, и об этом сказано в предупреждении.
extra_total=0; extra_bad=0
set -f
for net in $EXTRA_RU_NETS; do
    extra_total=$((extra_total + 1))
    ipset add ru_tmp "$net" -exist || {
        echo "WARN: EXTRA_RU_NETS: не добавил '$net' - ожидается адрес или сеть в формате CIDR" >&2
        extra_bad=$((extra_bad + 1))
    }
done
set +f
ipset swap ru_tmp ru
ipset destroy ru_tmp

# 3) Таблица + правило: помеченный трафик уходит на выход через awg1 (по номеру таблицы, rt_tables не нужен).
ip route replace default dev "$AWG1_IF" table "$TABLE_ID"
ip rule del fwmark "$FWMARK" table "$TABLE_ID" 2>/dev/null || true
ip rule add fwmark "$FWMARK" table "$TABLE_ID" priority "$RULE_PRIO"

# 4) Маршрут к самому AWG1 держим вне туннеля (иначе пакеты к нему закольцуются в awg1). replace = идемпотентно.
if [ -n "$WAN_GW" ]; then
    # На VPS со шлюзом вне подсети сервера (напр. Hetzner, интерфейс /32) обычный replace падает
    # с "Nexthop has invalid gateway" - тогда повторяем с onlink (шлюз доступен прямо на интерфейсе).
    ip route replace "$AWG1_ENDPOINT" via "$WAN_GW" dev "$WAN_IF" 2>/dev/null \
        || ip route replace "$AWG1_ENDPOINT" via "$WAN_GW" dev "$WAN_IF" onlink
else
    ip route replace "$AWG1_ENDPOINT" dev "$WAN_IF"
fi

# Пересылку (FORWARD) и NAT прямого RU-трафика (-o WAN) уже настроил установщик в awg0.conf,
# обратный трафик пропускает UFW по RELATED,ESTABLISHED. Здесь добавляем только маркировку и NAT на awg1.

# 5) Маркировка трафика клиентской подсети, входящего через awg0: RU-сети напрямую (RETURN), остальное метим.
#    -s CLIENT_SUBNET - чтобы не задеть другой трафик, если позже добавишь ещё подсеть/пир.
#    RETURN должен стоять перед MARK; -I ... 1 держит его первым, -C делает шаг идемпотентным.
iptables -t mangle -C PREROUTING -i awg0 -s "$CLIENT_SUBNET" -m set --match-set ru dst -j RETURN 2>/dev/null \
    || iptables -t mangle -I PREROUTING 1 -i awg0 -s "$CLIENT_SUBNET" -m set --match-set ru dst -j RETURN
iptables -t mangle -C PREROUTING -i awg0 -s "$CLIENT_SUBNET" -j MARK --set-mark "$FWMARK" 2>/dev/null \
    || iptables -t mangle -A PREROUTING -i awg0 -s "$CLIENT_SUBNET" -j MARK --set-mark "$FWMARK"

# 6) NAT для трафика клиентов, уходящего на выход через awg1 (для прямого RU NAT -o WAN уже есть от установщика).
iptables -t nat -C POSTROUTING -s "$CLIENT_SUBNET" -o "$AWG1_IF" -j MASQUERADE 2>/dev/null \
    || iptables -t nat -A POSTROUTING -s "$CLIENT_SUBNET" -o "$AWG1_IF" -j MASQUERADE

# Финальная строка не должна лгать: если часть своих адресов не добавилась, они пойдут
# в дальнее плечо, и молчаливое "OK" увело бы разбор не туда.
if [ "$extra_bad" -gt 0 ]; then
    echo "OK С ОГОВОРКОЙ: каскад применён, но из EXTRA_RU_NETS не добавлено $extra_bad из $extra_total - эти адреса пойдут через заграничное плечо" >&2
else
    echo "OK: каскадный роутинг применён (WAN=$WAN_IF, gw=${WAN_GW:-on-link}, выход=$AWG1_IF, table=$TABLE_ID)"
fi
ROUTING_EOF
sed -i -e "s|^CLIENT_SUBNET=\"[^\"]*\"|CLIENT_SUBNET=\"$CLIENT_SUBNET\"|" \
       -e "s|^AWG1_ENDPOINT=\"[^\"]*\"|AWG1_ENDPOINT=\"$EP_HOST\"|" "$ROUTING"
chmod +x "$ROUTING"

# ---------- 4. Автозапуск ----------
say "Настраиваю автозапуск после перезагрузки"
cat > "$UNIT" <<'UNIT_EOF'
[Unit]
Description=Cascade split-routing for AWG0
After=awg-quick@awg0.service awg-quick@awg1.service network-online.target
Requires=awg-quick@awg0.service awg-quick@awg1.service
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/root/awg/awg-routing.sh

[Install]
WantedBy=multi-user.target
UNIT_EOF
systemctl daemon-reload
systemctl enable awg-quick@awg1 awg-routing >/dev/null 2>&1

# Раз в неделю (пн, 05:00) обновлять список российских сетей
echo '0 5 * * 1 root systemctl restart awg-routing' > "$CRON"

# ---------- 5. Запуск маршрутизации ----------
say "Включаю раздельную маршрутизацию (скачивание списка RU-сетей, до минуты)"
systemctl restart awg-routing || die "маршрутизация не применилась — смотрите: journalctl -u awg-routing -n 50"
journalctl -u awg-routing -n 5 --no-pager -o cat 2>/dev/null | grep -E '^(OK|WARN|ERROR)' || true

# ---------- 6. Самопроверка ----------
say "Проверка"
entries=$(ipset list ru 2>/dev/null | awk -F': ' '/Number of entries/{print $2}' || true)
check() { if eval "$2" >/dev/null 2>&1; then echo "  [OK]   $1"; else echo "  [FAIL] $1"; fail=1; fi; }
fail=0
check "туннель awg1 к серверу EU"                 "awg show awg1 latest-handshakes | awk '{exit !(\$2>0)}'"
check "список RU-сетей загружен (${entries:-0} шт.)" "[ \"${entries:-0}\" -gt 1000 ]"
check "российский адрес (Яндекс) в списке прямых" "ipset test ru 77.88.55.242"
check "остальной трафик идёт через EU"            "ip route get 1.1.1.1 mark 0x1 | grep -q awg1"
check "автозапуск включён"                        "systemctl is-enabled awg-quick@awg1 && systemctl is-enabled awg-routing"

if [ "$fail" = 0 ]; then
    say "Каскад работает."
    echo "Клиентов (телефоны, ПК) добавляйте на ЭТОМ сервере: bash $AWG_DIR/manage_amneziawg.sh add <имя>"
    echo "Проверка с клиента: https://ifconfig.me покажет IP сервера EU ($EP_HOST), а ya.ru откроется напрямую."
else
    warn "часть проверок не прошла — см. раздел «Если что-то не так» в инструкции"
    exit 1
fi
