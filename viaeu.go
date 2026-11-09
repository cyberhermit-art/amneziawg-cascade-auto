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
	"strconv"
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
		"auto":    kv["AUTOON"] == "1", "autoCount": kv["AUTOCNT"], "autoDate": kv["AUTODATE"],
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
	body := ""
	if len(lines) > 0 {
		body = strings.Join(lines, "\n") + "\n"
	}
	// пустой список при включённом автосписке не выключает «Через EU» целиком
	cmd := fmt.Sprintf("printf '%%s' %s > /root/awg/awgc-eu.txt && bash %s apply 2>&1", shq(body), euScriptPath)
	out, se, code, err := m.ruCmd(cmd, nil, 2*time.Minute)
	if err != nil {
		return nil, err
	}
	if code != 0 || !strings.Contains(out, "@@OK") {
		return nil, fmt.Errorf("не получилось: %s", tail(se+out, 4))
	}
	return map[string]any{"ok": true, "nets": len(nets)}, nil
}

// handleEUAuto включает или выключает автосписок заблокированных в РФ адресов
// из российских сетей (antifilter.download, обновление раз в неделю).
func handleEUAuto(m *mctx, r exclReq) (any, error) {
	if err := m.uploadEUScript(); err != nil {
		return nil, err
	}
	cmd := "auto-off"
	if r.Enable {
		cmd = "auto-on"
	}
	out, se, _, err := m.ruCmd("bash "+euScriptPath+" "+cmd+" 2>&1", nil, 6*time.Minute)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(out, "@@OK") {
		return nil, fmt.Errorf("не получилось: %s", tail(se+out, 3))
	}
	n, _ := strconv.Atoi(strings.TrimSpace(kvLines(out)["AUTO"]))
	return map[string]any{"ok": true, "count": n}, nil
}

func registerViaEU(mux *http.ServeMux) {
	mux.HandleFunc("/api/m/eu-auto", xapi(handleEUAuto))
	mux.HandleFunc("/api/m/eu-get", xapi(handleEUGet))
	mux.HandleFunc("/api/m/eu-set", xapi(handleEUSet))
}
