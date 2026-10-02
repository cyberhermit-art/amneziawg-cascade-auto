// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

// Обновление скрипта маршрутизации на уже работающем каскаде: свежая версия
// из программы с сохранением настроек сервера (подсеть, адрес EU, свои сети).

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRUZoneURL = "https://www.ipdeny.com/ipblocks/data/aggregated/ru-aggregated.zone"
	extraRUZoneURL   = "https://raw.githubusercontent.com/antonme/GeoIP/master/russia.ipv4.cidr.txt"
)

// routingTemplate — awg-routing.sh из встроенного cascade-setup.sh.
func routingTemplate() (string, error) {
	s := string(cascadeScript)
	a := strings.Index(s, "<<'ROUTING_EOF'\n")
	if a < 0 {
		return "", errors.New("шаблон маршрутизации не найден")
	}
	s = s[a+len("<<'ROUTING_EOF'\n"):]
	b := strings.Index(s, "\nROUTING_EOF\n")
	if b < 0 {
		return "", errors.New("шаблон маршрутизации не найден")
	}
	return s[:b+1], nil
}

func setVar(script, name, value string) string {
	re := regexp.MustCompile(`(?m)^` + name + `="[^"\n]*"`)
	return re.ReplaceAllLiteralString(script, name+`="`+value+`"`)
}

var safeVal = regexp.MustCompile(`^[A-Za-z0-9 ./:_-]*$`)

func mRoutingUpdate(m *mctx) (any, error) {
	out, _, code, err := m.ruCmd(`F=/root/awg/awg-routing.sh; [ -f $F ] || exit 3
for v in CLIENT_SUBNET AWG1_IF AWG1_ENDPOINT EXTRA_RU_NETS RU_ZONE_URL; do printf '@@%s %s\n' "$v" "$(grep -oP "^$v=\"\K[^\"]*" $F | head -1)"; done`, nil, time.Minute)
	if err != nil {
		return nil, err
	}
	if code == 3 {
		return nil, errors.New("на сервере RU нет скрипта маршрутизации — каскад не настроен")
	}
	vals := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "@@") {
			k, v, _ := strings.Cut(strings.TrimPrefix(l, "@@"), " ")
			vals[k] = strings.TrimSpace(v)
		}
	}
	if vals["CLIENT_SUBNET"] == "" || vals["AWG1_ENDPOINT"] == "" || vals["AWG1_ENDPOINT"] == "CHANGE_ME" {
		return nil, errors.New("не удалось прочитать настройки текущей маршрутизации")
	}
	for k, v := range vals {
		if !safeVal.MatchString(v) {
			return nil, fmt.Errorf("необычное значение %s в текущем скрипте — обновите вручную", k)
		}
	}
	tpl, err := routingTemplate()
	if err != nil {
		return nil, err
	}
	tpl = setVar(tpl, "CLIENT_SUBNET", vals["CLIENT_SUBNET"])
	tpl = setVar(tpl, "AWG1_ENDPOINT", vals["AWG1_ENDPOINT"])
	if vals["AWG1_IF"] != "" {
		tpl = setVar(tpl, "AWG1_IF", vals["AWG1_IF"])
	}
	tpl = setVar(tpl, "EXTRA_RU_NETS", vals["EXTRA_RU_NETS"])
	// Свой основной источник сохраняем; если вручную был поставлен список MaxMind —
	// он теперь подключается как дополнительный, а основным снова идёт ipdeny.
	if u := vals["RU_ZONE_URL"]; u != "" && u != defaultRUZoneURL && !strings.Contains(u, "antonme/GeoIP") {
		tpl = setVar(tpl, "RU_ZONE_URL", u)
	}
	out, se, code, err := m.ruCmd(`set -e
F=/root/awg/awg-routing.sh
cp -p $F $F.bak-$(date +%Y%m%d-%H%M%S)
cat > $F.new && chmod 700 $F.new && mv -f $F.new $F
systemctl restart awg-routing
echo "@@NETS $(ipset list ru 2>/dev/null | awk -F': ' '/Number of entries/{print $2}')"
journalctl -u awg-routing -n 8 --no-pager -o cat 2>/dev/null | grep -E '^(OK|WARN|ERROR)' || true`, []byte(tpl), 4*time.Minute)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("маршрутизация не применилась: %s", tail(se+out, 4))
	}
	nets := 0
	var notes []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "@@NETS ") {
			nets, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(l, "@@NETS ")))
		} else if l = strings.TrimSpace(l); l != "" {
			notes = append(notes, l)
		}
	}
	return map[string]any{"ok": true, "nets": nets, "log": notes}, nil
}
