// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

// Несколько выходов с автоматическим переключением (полная версия, 1.15).
//
// «Узел выбора выхода» — сервер, который подключается к выходам: сервер RU в
// каскаде из двух серверов, транзит в каскаде из трёх. На нём к основному
// туннелю awg1 добавляются резервные (awg2, awg3) и, для трёх серверов, WARP;
// assets/awgc-exits.sh раз в 30 секунд проверяет их и переключает зарубежный
// трафик на живой выход. Резервный выход программа ставит так же, как
// обычный: обновление системы, AmneziaWG той же версии, клиент для узла.

import (
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed assets/awgc-exits.sh
var exitsScript []byte

const exitsPath = "/root/awg/awgc-exits.sh"

// hopRole — роль узла выбора выхода в профиле.
func hopRole(p *Profile) string {
	if p.EX.Host != "" {
		return "EU"
	}
	return "RU"
}

// exitClientName — как на выходе называется клиент узла выбора выхода.
func exitClientName(p *Profile) string {
	if p.EX.Host != "" {
		return "mid_host"
	}
	return "ru_host"
}

func exitSubnet(iface string) string {
	n, _ := strconv.Atoi(strings.TrimPrefix(iface, "awg"))
	return fmt.Sprintf("172.16.%d.1/24", 69+n) // awg2 → 172.16.71.1/24, awg3 → 172.16.72.1/24
}

func freeExitIface(p *Profile) string {
	used := map[string]bool{}
	for _, e := range p.Exits {
		used[e.Iface] = true
	}
	for _, i := range []string{"awg2", "awg3"} {
		if !used[i] {
			return i
		}
	}
	return ""
}

type exitRow struct {
	Iface    string `json:"iface"`
	Label    string `json:"label"`
	Host     string `json:"host"`
	Endpoint string `json:"endpoint"`
	Up       bool   `json:"up"`
	HSAge    int64  `json:"hsAge"`
	OK       int    `json:"ok"`   // удачных проверок подряд
	Fail     int    `json:"fail"` // неудачных проверок подряд
	Current  bool   `json:"current"`
}

func handleExitsGet(m *mctx, _ exitsReq) (any, error) {
	hop := hopRole(m.p)
	c, err := serverConn(m.p, hop)
	if err != nil {
		return nil, err
	}
	if err := c.upload(exitsPath, exitsScript, "700"); err != nil {
		return nil, fmt.Errorf("сервер %s: %v", hop, err)
	}
	out, _, _, err := c.lockedRun(`bash `+exitsPath+` status`, nil, time.Minute)
	if err != nil {
		return nil, fmt.Errorf("сервер %s: %v", hop, err)
	}
	kv := kvLines(out)
	now, _ := strconv.ParseInt(kv["NOW"], 10, 64)
	cur := kv["CUR"]
	hosts := map[string]string{"awg1": m.p.EU.Host}
	if m.p.EX.Host != "" {
		hosts["awg1"] = m.p.EX.Host
	}
	for _, e := range m.p.Exits {
		hosts[e.Iface] = e.Host
	}
	rows := []exitRow{}
	var sw []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "@@SW ") {
			sw = append(sw, strings.TrimPrefix(l, "@@SW "))
		}
		if !strings.HasPrefix(l, "@@EXIT ") {
			continue
		}
		f := strings.Split(strings.TrimPrefix(l, "@@EXIT "), "|")
		if len(f) < 7 {
			continue
		}
		hs, _ := strconv.ParseInt(f[2], 10, 64)
		age := int64(-1)
		if hs > 0 && now > 0 {
			age = now - hs
		}
		ok, _ := strconv.Atoi(f[5])
		fl, _ := strconv.Atoi(f[6])
		h := hosts[f[0]]
		if f[0] == "warp" {
			h = "Cloudflare WARP"
		}
		rows = append(rows, exitRow{Iface: f[0], Label: f[1], Host: h, Endpoint: f[3], Up: f[4] == "1", HSAge: age, OK: ok, Fail: fl, Current: f[0] == cur})
	}
	return map[string]any{
		"hop": hop, "threeHop": m.p.EX.Host != "", "current": cur, "rows": rows, "switches": sw,
		"timer": kv["TIMER"] == "active", "canAdd": freeExitIface(m.p) != "",
	}, nil
}

func handleExitsRemove(m *mctx, r exitsReq) (any, error) {
	if r.Iface != "awg2" && r.Iface != "awg3" && r.Iface != "warp" {
		return nil, errors.New("этот выход так не удаляется")
	}
	hop := hopRole(m.p)
	c, err := serverConn(m.p, hop)
	if err != nil {
		return nil, err
	}
	if err := c.upload(exitsPath, exitsScript, "700"); err != nil {
		return nil, fmt.Errorf("сервер %s: %v", hop, err)
	}
	out, se, _, err := c.lockedRun(`bash `+exitsPath+` remove `+r.Iface, nil, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("сервер %s: %v", hop, err)
	}
	if !strings.Contains(out, "@@OK") {
		return nil, fmt.Errorf("сервер %s: %s", hop, tail(se+out, 3))
	}
	note := ""
	var keep []ExitCfg
	for _, e := range m.p.Exits {
		if e.Iface != r.Iface {
			keep = append(keep, e)
			continue
		}
		// на самом резервном сервере убираем клиента узла (по возможности)
		rc := NewRemote(e.ServerCfg, func(string) {})
		if err := rc.Connect(); err == nil {
			_, _, _ = rc.Output(`bash `+manageSh+` --yes --no-color remove `+exitClientName(m.p)+` >/dev/null 2>&1; true`, nil, 2*time.Minute)
			rc.Close()
		} else {
			note = "Резервный сервер недоступен — на нём остался клиент " + exitClientName(m.p) + "; AmneziaWG на нём можно удалить вручную или просто удалить VPS."
		}
	}
	if err := setProfileExits(m.p.ID, keep); err != nil {
		return nil, err
	}
	logLine(fmt.Sprintf("[Выходы] «%s»: удалён выход %s", m.p.Name, r.Iface))
	return map[string]any{"ok": true, "note": note}, nil
}

func handleExitsWarp(m *mctx, r exitsReq) (any, error) {
	if m.p.EX.Host == "" {
		return nil, errors.New("WARP как запасной выход доступен только в каскаде из трёх серверов: из России протокол WARP блокируется, а транзит находится за границей")
	}
	c, err := serverConn(m.p, "EU")
	if err != nil {
		return nil, err
	}
	if err := c.upload(exitsPath, exitsScript, "700"); err != nil {
		return nil, fmt.Errorf("транзит: %v", err)
	}
	cmd := "warp-off"
	if r.Enable {
		cmd = "warp-on"
	}
	out, se, _, err := c.lockedRun(`bash `+exitsPath+` `+cmd+` 2>&1`, nil, 3*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("транзит: %v", err)
	}
	switch {
	case strings.Contains(out, "@@OK"):
		logLine(fmt.Sprintf("[Выходы] «%s»: WARP %s", m.p.Name, map[bool]string{true: "включён", false: "выключен"}[r.Enable]))
		return map[string]any{"ok": true}, nil
	case strings.Contains(out, "@@NOPROBE"):
		return map[string]any{"ok": true, "note": "Туннель WARP поднят, но проверка связи через него пока не прошла — возможно, Cloudflare недоступен с транзита. Переключатель будет проверять его каждые 30 секунд."}, nil
	}
	return nil, fmt.Errorf("транзит: %s", tail(se+out, 3))
}

type exitsReq struct {
	ID     string `json:"id"`
	Iface  string `json:"iface"`
	Enable bool   `json:"enable"`
}

func registerExits(mux *http.ServeMux) {
	mux.HandleFunc("/api/m/exits-get", papi(handleExitsGet))
	mux.HandleFunc("/api/m/exits-remove", papi(handleExitsRemove))
	mux.HandleFunc("/api/m/exits-warp", papi(handleExitsWarp))
}

// ---------- установка резервного выхода (задача, как установка каскада) ----------

func (in *JobInput) validateAddExit() (*Profile, error) {
	p, err := getProfile(in.ProfileID)
	if err != nil {
		return nil, err
	}
	in.RX.Role = "RX"
	in.RX.Host = strings.TrimSpace(in.RX.Host)
	if in.RX.Host == "" {
		return nil, errors.New("не указан IP-адрес резервного выхода")
	}
	if in.RX.Port == 0 {
		in.RX.Port = 22
	}
	if in.RX.Password == "" && strings.TrimSpace(in.RX.Key) == "" {
		return nil, errors.New("укажите пароль или SSH-ключ резервного выхода")
	}
	for _, h := range []string{p.RU.Host, p.EU.Host, p.EX.Host} {
		if h != "" && h == in.RX.Host {
			return nil, errors.New("это адрес сервера, который уже есть в каскаде")
		}
	}
	for _, e := range p.Exits {
		if e.Host == in.RX.Host {
			return nil, errors.New("этот сервер уже добавлен как резервный выход")
		}
	}
	hop := hopRole(p)
	h := cfgOf(p, hop)
	if h.Password == "" && strings.TrimSpace(h.Key) == "" {
		return nil, fmt.Errorf("в профиле нет данных для входа на сервер %s — он нужен, чтобы подключить резервный выход", hop)
	}
	if freeExitIface(p) == "" {
		return nil, errors.New("резервных выходов уже два — больше не поддерживается")
	}
	return p, nil
}

func (j *Job) runAddExit(res *FinalResult) error {
	p, err := getProfile(j.in.ProfileID)
	if err != nil {
		return err
	}
	res.Profile, res.Mode = p.ID, "add-exit"
	hopR := hopRole(p)
	hcfg := cfgOf(p, hopR)
	hcfg.Role = hopR
	hop := NewRemote(hcfg, func(s string) { j.hub.Log(hopR, s) })
	rx := NewRemote(j.in.RX, func(s string) { j.hub.Log("RX", s) })
	defer hop.Close()
	defer rx.Close()
	iface := freeExitIface(p)
	client := exitClientName(p)

	if err := j.stage("connect", func() error {
		if err := j.precheck(hop); err != nil {
			return err
		}
		return j.precheck(rx)
	}); err != nil {
		return err
	}
	proto, err := j.serverProto(hop)
	if err != nil {
		return err
	}
	if err := j.updateStage("rx-update", rx); err != nil {
		return err
	}
	if err := j.stage("rx-install", func() error {
		if err := j.installServer(rx, exitSubnet(iface), proto); err != nil {
			return err
		}
		got, err := j.serverProto(rx)
		if err != nil {
			return err
		}
		if got != proto {
			return fmt.Errorf("на резервном выходе AmneziaWG %s, а на сервере %s — %s. Туннель не заработает: удалите AmneziaWG с резервного сервера и попробуйте снова", got, hopR, proto)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := j.stage("rx-link", func() error {
		j.logf("RX", "Создаю на резервном выходе подключение %s…", client)
		out, code, err := j.exec(rx, fmt.Sprintf(`[ -f /root/awg/%[1]s.conf ] || bash /root/awg/manage_amneziawg.sh add %[1]s >/tmp/%[1]s_add.log 2>&1 || { cat /tmp/%[1]s_add.log; exit 1; }; cat /root/awg/%[1]s.conf`, client), nil, 5*time.Minute)
		if err != nil {
			return err
		}
		if code != 0 || !strings.Contains(out, "[Peer]") {
			j.hub.Log("RX", out)
			return fmt.Errorf("резервный выход: не удалось создать клиента %s", client)
		}
		src := "/root/awg/awgc-exit-" + iface + ".conf"
		if err := j.upload(hop, src, []byte(out), "600"); err != nil {
			return err
		}
		if err := j.upload(hop, exitsPath, exitsScript, "700"); err != nil {
			return err
		}
		label := map[string]string{"awg2": "резерв-1", "awg3": "резерв-2"}[iface]
		j.logf(hopR, "Подключаю резервный выход как %s и включаю переключение выходов…", iface)
		out, code, err = j.exec(hop, fmt.Sprintf(`bash %s add %s %s %s 2>&1`, exitsPath, iface, src, label), nil, 3*time.Minute)
		if err != nil {
			return err
		}
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "@@") {
				j.hub.Log(hopR, l)
			}
		}
		if code != 0 || (!strings.Contains(out, "@@OK") && !strings.Contains(out, "@@NOPROBE")) {
			return fmt.Errorf("сервер %s: не удалось подключить резервный выход", hopR)
		}
		if strings.Contains(out, "@@NOPROBE") {
			j.hub.Log(hopR, "Туннель к резервному выходу поднят, но проверка связи через него пока не прошла — переключатель будет проверять его каждые 30 секунд.")
		}
		exits := append(append([]ExitCfg{}, p.Exits...), ExitCfg{ServerCfg: j.in.RX, Iface: iface})
		return setProfileExits(p.ID, exits)
	}); err != nil {
		return err
	}
	j.hub.Log("", "Готово! Резервный выход подключён: если основной выход перестанет отвечать, зарубежный трафик переключится на него автоматически.")
	return nil
}
