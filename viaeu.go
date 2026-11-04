// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

// «Через EU»: российские сервисы, которые не пускают адрес сервера RU, но
// работают с зарубежного адреса, отправляются через сервер EU. Всё на сервере
// (assets/awgc-eu.sh), файлы подключения пользователей не меняются.

import (
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

//go:embed assets/awgc-eu.sh
var euScript []byte

const euScriptPath = "/root/awg/awgc-eu.sh"

// Готовые наборы для «Через EU».
var euPresets = []struct {
	ID, Title string
	Nets      []string
}{
	// Сети автономных систем Wildberries (AS57073, AS201512, AS49053 и др.) по данным iptoasn, ноябрь 2026.
	{"wildberries", "Wildberries (сайт, приложение, картинки)", []string{"85.198.76.0/22", "90.156.244.0/22", "91.230.107.0/24", "176.101.88.0/21", "185.62.200.0/22", "185.138.252.0/22", "194.1.214.0/24", "213.184.154.0/23", "213.184.156.0/22"}},
}

func (m *mctx) uploadEUScript() error {
	if err := getConn(m.p, "RU").upload(euScriptPath, euScript, "700"); err != nil {
		return fmt.Errorf("сервер RU: %v", err)
	}
	return nil
}

func handleEUGet(m *mctx, _ exclReq) (any, error) {
	if err := m.uploadEUScript(); err != nil {
		return nil, err
	}
	out, _, _, err := m.ruCmd("bash "+euScriptPath+" status", nil, time.Minute)
	if err != nil {
		return nil, err
	}
	kv := kvLines(out)
	pl := []map[string]any{}
	for _, p := range euPresets {
		pl = append(pl, map[string]any{"id": p.ID, "title": p.Title, "nets": p.Nets})
	}
	return map[string]any{
		"cascade": kv["CASCADE"] == "1",
		"active":  kv["ACTIVE"] == "1",
		"first":   kv["FIRST"] == "1",
		"nets":    strings.TrimSpace(between(out, "@@NETS", "@@END")),
		"presets": pl,
	}, nil
}

func handleEUSet(m *mctx, r exclReq) (any, error) {
	nets, err := parseNets(r.Nets)
	if err != nil {
		return nil, err
	}
	if len(nets) > 500 {
		return nil, errors.New("слишком много сетей (больше 500)")
	}
	if err := m.uploadEUScript(); err != nil {
		return nil, err
	}
	var lines []string
	for _, p := range nets {
		lines = append(lines, p.String())
	}
	cmd := fmt.Sprintf("printf '%%s' %s > /root/awg/awgc-eu.txt && bash %s apply 2>&1", shq(strings.Join(lines, "\n")+"\n"), euScriptPath)
	if len(nets) == 0 {
		cmd = "bash " + euScriptPath + " disable 2>&1"
	}
	out, se, code, err := m.ruCmd(cmd, nil, 2*time.Minute)
	if err != nil {
		return nil, err
	}
	if code != 0 || !strings.Contains(out, "@@OK") {
		return nil, fmt.Errorf("не получилось: %s", tail(se+out, 4))
	}
	return map[string]any{"ok": true, "nets": len(nets)}, nil
}

func registerViaEU(mux *http.ServeMux) {
	mux.HandleFunc("/api/m/eu-get", xapi(handleEUGet))
	mux.HandleFunc("/api/m/eu-set", xapi(handleEUSet))
}
