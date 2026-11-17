// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

// Массовое обновление файлов подключения («Пользователи» → «Обновить файлы
// подключения»). Ключи пользователей не меняются: на сервере пересобираются
// .conf, QR-код и ссылка vpn:// с текущими настройками («Мимо VPN», адрес и
// параметры сервера), затем файлы сохраняются в папку профиля на компьютере.

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type regenReq struct {
	ID    string   `json:"id"`
	Names []string `json:"names"` // пусто — все пользователи
	Force bool     `json:"force"` // пересобрать файлы даже там, где они в порядке
}

type regenRow struct {
	Name   string `json:"name"`
	Action string `json:"action"` // routes — обновлены маршруты; rebuilt — файлы пересобраны; same — уже в порядке; fail
	Error  string `json:"error,omitempty"`
	QR     bool   `json:"qr"`
}

// regenScript обходит пользователей и приводит их файлы к текущим настройкам:
//   - AllowedIPs отличается от настройки «Мимо VPN» → modify (он сам пересоберёт QR и vpn://);
//   - нет QR или vpn:// (или force) → regen: файлы с сервера, список маршрутов сохраняется.
const regenScript = `set -u
A=/root/awg; M=%[1]s; C=/etc/amnezia/amneziawg/awg0.conf
T=""; if [ -s %[2]s ]; then T=$(cat %[2]s); elif [ -s %[3]s ]; then T=$(cat %[3]s); fi
FORCE=%[4]d
norm() { tr -d ' \t\r\n'; }
for n in %[5]s; do
  grep -qxF "#_Name = $n" $C || { echo "@@FAIL $n нет на сервере"; continue; }
  act=same
  if [ "$FORCE" = 1 ] || [ ! -f $A/$n.conf ] || [ ! -f $A/$n.png ] || [ ! -f $A/$n.vpnuri ]; then
    if bash $M --yes --no-color regen "$n" >/tmp/awgc-rg.log 2>&1; then act=rebuilt
    else echo "@@FAIL $n $(tail -1 /tmp/awgc-rg.log)"; continue; fi
  fi
  if [ -n "$T" ]; then
    cur=$(sed -n -E 's/^AllowedIPs[[:space:]]*=[[:space:]]*//p' $A/$n.conf 2>/dev/null | head -1)
    if [ "$(printf %%s "$cur" | norm)" != "$(printf %%s "$T" | norm)" ]; then
      if bash $M --yes --no-color modify "$n" AllowedIPs "$T" >/tmp/awgc-rg.log 2>&1; then act=routes
      else echo "@@FAIL $n $(tail -1 /tmp/awgc-rg.log)"; continue; fi
    fi
  fi
  echo "@@DONE $n $act"
done
echo "@@END"`

func handleRegenFiles(m *mctx, r regenReq) (any, error) {
	m.prepareRU()
	names := r.Names
	if len(names) == 0 { // все пользователи
		out, _, _, err := m.ruCmd(`grep '^#_Name = ' /etc/amnezia/amneziawg/awg0.conf | sed 's/^#_Name = //'`, nil, time.Minute)
		if err != nil {
			return nil, err
		}
		names = strings.Fields(out)
	}
	var list []string
	seen := map[string]bool{}
	for _, n := range names {
		if n == "ru_host" || n == "mid_host" || seen[n] {
			continue
		}
		if err := checkName(n); err != nil {
			return nil, fmt.Errorf("%s: %v", n, err)
		}
		seen[n] = true
		list = append(list, n)
	}
	if len(list) == 0 {
		return nil, errors.New("на сервере нет пользователей")
	}
	sort.Strings(list)
	force := 0
	if r.Force {
		force = 1
	}
	out, se, _, err := m.ruCmd(fmt.Sprintf(regenScript, manageSh, exclAllowed, exclBase, force, strings.Join(list, " ")),
		nil, time.Duration(30+20*len(list))*time.Second)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(out, "@@END") {
		return nil, fmt.Errorf("сервер RU: %s", tail(se+out, 4))
	}
	rows := map[string]*regenRow{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimSpace(l), " ", 3)
		if len(f) < 2 || !seen[f[1]] {
			continue
		}
		switch f[0] {
		case "@@DONE":
			rows[f[1]] = &regenRow{Name: f[1], Action: strings.TrimSpace(strings.Join(f[2:], ""))}
		case "@@FAIL":
			rows[f[1]] = &regenRow{Name: f[1], Action: "fail", Error: strings.TrimSpace(clean(strings.Join(f[2:], "")))}
		}
	}

	// Файлы — в папку профиля, как после установки.
	dir := filepath.Join(profileDir(m.p), "файлы подключения "+time.Now().Format("2006-01-02_15-04"))
	saveErr := ""
	if err := os.MkdirAll(dir, 0o700); err != nil {
		saveErr = err.Error()
	}
	res := []regenRow{}
	saved := 0
	for _, n := range list {
		row := rows[n]
		if row == nil {
			row = &regenRow{Name: n, Action: "fail", Error: "сервер не ответил по этому пользователю"}
		}
		if row.Action != "fail" && saveErr == "" {
			cf, err := m.fetchClient(n)
			if err != nil {
				row.Action, row.Error = "fail", err.Error()
			} else {
				_ = os.WriteFile(filepath.Join(dir, n+".conf"), []byte(cf.Conf), 0o600)
				if cf.VpnURI != "" {
					_ = os.WriteFile(filepath.Join(dir, n+".vpnuri.txt"), []byte(cf.VpnURI+"\n"), 0o600)
				}
				if b, err := base64.StdEncoding.DecodeString(cf.PNG); err == nil && len(b) > 0 {
					_ = os.WriteFile(filepath.Join(dir, n+"-qr.png"), b, 0o600)
					row.QR = true
				}
				saved++
			}
		}
		res = append(res, *row)
	}
	if saved == 0 {
		_ = os.Remove(dir) // пустую папку не оставляем
		dir = ""
	}
	logLine(fmt.Sprintf("[Файлы подключения] «%s»: обновлено %d из %d", m.p.Name, saved, len(list)))
	return map[string]any{"rows": res, "folder": dir, "saved": saved, "saveError": saveErr}, nil
}

func registerRegenFiles(mux *http.ServeMux) {
	mux.HandleFunc("/api/m/regen-files", papi(handleRegenFiles))
}
