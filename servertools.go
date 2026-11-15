// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

// Инструменты серверов (1.17, полная версия):
//   - второй IP для выхода (assets/awgc-exitip.sh): туннель приходит на
//     основной адрес выхода, а в интернет трафик уходит со второго;
//   - смена порта AmneziaWG (manage port из установщика 6.3.0+) с
//     перенастройкой того, кто к этому серверу подключается;
//   - исправления безопасности для серверов, поставленных до установщика
//     6.1.2/6.2.1: защита от CVE-2026-98432 (--ipopts-guard) и обновление apt
//     без повторных загрузок (CVE-2026-109518).

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed assets/awgc-exitip.sh
var exitipScript []byte

const exitipPath = "/root/awg/awgc-exitip.sh"

type toolReq struct {
	ID     string `json:"id"`
	Target string `json:"target"` // ru | eu | ex
	IP     string `json:"ip"`     // второй IP; пусто — выключить
	Port   int    `json:"port"`
}

// exitRole — сервер, с которого трафик выходит в интернет.
func exitRole(p *Profile) string {
	if p.EX.Host != "" {
		return "EX"
	}
	return "EU"
}

// ---------- второй IP для выхода ----------

func handleExitIPGet(m *mctx, _ toolReq) (any, error) {
	role := exitRole(m.p)
	c, err := serverConn(m.p, role)
	if err != nil {
		return nil, err
	}
	if err := c.upload(exitipPath, exitipScript, "700"); err != nil {
		return nil, fmt.Errorf("сервер %s: %v", role, err)
	}
	out, _, _, err := c.lockedRun(`bash `+exitipPath+` status`, nil, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("сервер %s: %v", role, err)
	}
	kv := kvLines(out)
	return map[string]any{"role": role, "main": kv["MAIN"], "ip": kv["IP"], "active": kv["RULE"] == "1"}, nil
}

func handleExitIPSet(m *mctx, r toolReq) (any, error) {
	role := exitRole(m.p)
	c, err := serverConn(m.p, role)
	if err != nil {
		return nil, err
	}
	if err := c.upload(exitipPath, exitipScript, "700"); err != nil {
		return nil, fmt.Errorf("сервер %s: %v", role, err)
	}
	cmd := `bash ` + exitipPath + ` off 2>&1`
	ip := strings.TrimSpace(r.IP)
	if ip != "" {
		a := net.ParseIP(ip)
		if a == nil || a.To4() == nil || a.IsPrivate() || a.IsLoopback() {
			return nil, errors.New("укажите публичный IPv4-адрес, который хостер выдал этому серверу вторым")
		}
		for _, h := range []string{m.p.RU.Host, m.p.EU.Host, m.p.EX.Host} {
			if h == ip {
				return nil, errors.New("это адрес одного из серверов каскада — нужен дополнительный адрес выхода")
			}
		}
		cmd = `bash ` + exitipPath + ` on ` + ip + ` 2>&1`
	}
	out, se, _, err := c.lockedRun(cmd, nil, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("сервер %s: %v", role, err)
	}
	if !strings.Contains(out, "@@OK") {
		return nil, fmt.Errorf("сервер %s: %s", role, tail(se+out, 3))
	}
	// Проверка с другого конца: какой адрес видит интернет через каскад.
	seen := ""
	if ip != "" {
		so, _, _, err := m.ruCmd(`I=$(cat /root/awg/awgc-exits/current 2>/dev/null); ip link show "${I:-none}" >/dev/null 2>&1 || I=awg1
curl -s -m 15 --interface $I https://api.ipify.org 2>/dev/null || curl -s -m 15 --interface $I https://ifconfig.me/ip 2>/dev/null`, nil, time.Minute)
		if err == nil {
			seen = strings.TrimSpace(so)
		}
	}
	logLine(fmt.Sprintf("[Второй IP выхода] «%s»: %s", m.p.Name, map[bool]string{true: "включён " + ip, false: "выключен"}[ip != ""]))
	return map[string]any{"ok": true, "seen": seen}, nil
}

// ---------- смена порта ----------

var unknownCmdRe = regexp.MustCompile(`(?i)неизвестн|unknown command|usage:|использование:`)

func handlePortSet(m *mctx, r toolReq) (any, error) {
	role, err := roleOf(r.Target)
	if err != nil {
		return nil, err
	}
	if r.Port < 1 || r.Port > 65535 {
		return nil, errors.New("порт — число от 1 до 65535")
	}
	c, err := serverConn(m.p, role)
	if err != nil {
		return nil, err
	}
	readPort := `sed -n -E 's/^ListenPort[[:space:]]*=[[:space:]]*([0-9]+).*/\1/p' /etc/amnezia/amneziawg/awg0.conf | head -1`
	out, _, _, err := c.lockedRun(readPort, nil, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("сервер %s: %v", role, err)
	}
	old, _ := strconv.Atoi(strings.TrimSpace(out))
	if old == r.Port {
		return nil, errors.New("сервер уже работает на этом порту")
	}
	setPort := func(p int) (string, error) {
		o, se, code, err := c.lockedRun(fmt.Sprintf(`bash %s --yes --no-color port %d 2>&1`, manageSh, p), nil, 4*time.Minute)
		if err != nil {
			return "", err
		}
		txt := o + se
		if code != 0 {
			if unknownCmdRe.MatchString(txt) && !strings.Contains(txt, "ListenPort") {
				return txt, errors.New("на этом сервере старая версия скриптов AmneziaWG — команда смены порта появилась в установщике 6.3.0 (ноябрь 2026). Порт можно сменить переустановкой сервера")
			}
			return txt, fmt.Errorf("не получилось: %s", tail(txt, 3))
		}
		return txt, nil
	}
	if _, err := setPort(r.Port); err != nil {
		return nil, fmt.Errorf("сервер %s: %v", role, err)
	}
	// Кто подключается к этому серверу — тому новый порт в Endpoint.
	var peer, peerConf, extra string
	switch {
	case role == "EU":
		peer, peerConf, extra = "RU", "/etc/amnezia/amneziawg/awg1.conf", "/root/ru_host.conf"
	case role == "EX":
		peer, peerConf, extra = "EU", "/etc/amnezia/amneziawg/awg1.conf", "/root/mid_host.conf"
	}
	if peer == "" { // сервер RU: к нему подключаются пользователи
		logLine(fmt.Sprintf("[Порт] «%s»: RU %d → %d", m.p.Name, old, r.Port))
		return map[string]any{"ok": true, "old": old, "new": r.Port, "reimport": true}, nil
	}
	pc, err := serverConn(m.p, peer)
	if err != nil {
		_, _ = setPort(old)
		return nil, fmt.Errorf("порт на сервере %s возвращён: %v", role, err)
	}
	out, _, _, err = pc.lockedRun(fmt.Sprintf(`set -u
C=%[1]s; X=%[2]s; P=%[3]d
for f in $C $X; do [ -f $f ] && sed -i -E "s|^(Endpoint[[:space:]]*=[[:space:]]*[^:]+):[0-9]+|\1:$P|" $f; done
systemctl restart awg-quick@awg1
for i in $(seq 1 15); do
  ping -c1 -W1 -I awg1 1.1.1.1 >/dev/null 2>&1
  h=$(awg show awg1 latest-handshakes 2>/dev/null | awk 'NR==1{print $2}'); t=$(date +%%s)
  if [ -n "$h" ] && [ "$h" != 0 ] && [ $((t-h)) -lt 60 ]; then echo "@@OK"; exit 0; fi
  sleep 2
done
echo "@@NOHS"`, peerConf, extra, r.Port), nil, 2*time.Minute)
	if err != nil || !strings.Contains(out, "@@OK") {
		// откат: старый порт на обоих концах
		_, _ = setPort(old)
		_, _, _, _ = pc.lockedRun(fmt.Sprintf(`for f in %s %s; do [ -f $f ] && sed -i -E "s|^(Endpoint[[:space:]]*=[[:space:]]*[^:]+):[0-9]+|\1:%d|" $f; done; systemctl restart awg-quick@awg1`, peerConf, extra, old), nil, time.Minute)
		why := "туннель на новом порту не установил связь (возможно, порт закрыт у хостера)"
		if err != nil {
			why = err.Error()
		}
		return nil, fmt.Errorf("%s — прежний порт %d возвращён", why, old)
	}
	logLine(fmt.Sprintf("[Порт] «%s»: %s %d → %d", m.p.Name, role, old, r.Port))
	return map[string]any{"ok": true, "old": old, "new": r.Port}, nil
}

// ---------- исправления безопасности ----------

const secCheckCmd = `echo "@@IPG $(systemctl is-enabled amneziawg-ipopts-guard.service 2>/dev/null || echo no)"
echo "@@APT $(dpkg-query -W -f='${Version}' apt 2>/dev/null)"`

type secRow struct {
	Role  string `json:"role"`
	Guard bool   `json:"guard"`
	Apt   string `json:"apt"`
	Error string `json:"error,omitempty"`
}

func secServers(p *Profile) []string {
	out := []string{"RU"}
	for _, r := range []string{"EU", "EX"} {
		s := cfgOf(p, r)
		if s.Host != "" && (s.Password != "" || s.Key != "") {
			out = append(out, r)
		}
	}
	return out
}

func handleSecGet(m *mctx, _ toolReq) (any, error) {
	roles := secServers(m.p)
	rows := make([]secRow, len(roles))
	var wg sync.WaitGroup
	for i, role := range roles {
		wg.Add(1)
		go func(i int, role string) {
			defer wg.Done()
			row := secRow{Role: role}
			out, _, _, err := getConn(m.p, role).lockedRun(secCheckCmd, nil, 30*time.Second)
			if err != nil {
				row.Error = err.Error()
			} else {
				kv := kvLines(out)
				row.Guard = kv["IPG"] == "enabled"
				row.Apt = kv["APT"]
			}
			rows[i] = row
		}(i, role)
	}
	wg.Wait()
	return map[string]any{"rows": rows}, nil
}

func handleSecFix(m *mctx, r toolReq) (any, error) {
	role, err := roleOf(r.Target)
	if err != nil {
		return nil, err
	}
	c, err := serverConn(m.p, role)
	if err != nil {
		return nil, err
	}
	// свежий установщик: защита --ipopts-guard появилась в 6.1.2
	_, _, _, _ = c.lockedRun(`rm -f `+installerPath, nil, 30*time.Second)
	if err := c.ensureInstaller(); err != nil {
		return nil, fmt.Errorf("сервер %s: %v", role, err)
	}
	out, se, _, err := c.lockedRun(`export DEBIAN_FRONTEND=noninteractive
echo "== Защита от CVE-2026-98432 (TCP с IP-опциями)"
bash `+installerPath+` --yes --ipopts-guard 2>&1 | tail -6
echo "== Обновление apt без повторных загрузок (CVE-2026-109518)"
apt-get -q -o Acquire::Retries=0 -o DPkg::Lock::Timeout=600 update >/dev/null 2>&1 && apt-get -q -y -o Acquire::Retries=0 -o DPkg::Lock::Timeout=600 install --only-upgrade apt 2>&1 | tail -3
`+secCheckCmd, nil, 10*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("сервер %s: %v", role, err)
	}
	kv := kvLines(out)
	var log []string
	for _, l := range strings.Split(out+se, "\n") {
		if l = strings.TrimSpace(clean(l)); l != "" && !strings.HasPrefix(l, "@@") {
			log = append(log, l)
		}
	}
	logLine(fmt.Sprintf("[Безопасность] «%s»: сервер %s — защита ipopts: %s, apt %s", m.p.Name, role, kv["IPG"], kv["APT"]))
	return map[string]any{"ok": kv["IPG"] == "enabled", "guard": kv["IPG"] == "enabled", "apt": kv["APT"], "log": log}, nil
}

func tapi2(f func(m *mctx, r toolReq) (any, error)) func(w http.ResponseWriter, r *http.Request) {
	return api(func(w http.ResponseWriter, r *http.Request) {
		var q toolReq
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&q); err != nil {
			writeJSON(w, map[string]string{"error": "неверный запрос"})
			return
		}
		p, err := getProfile(q.ID)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		res, err := f(&mctx{p: p}, q)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, res)
	})
}

func registerServerTools(mux *http.ServeMux) {
	mux.HandleFunc("/api/m/exitip-get", tapi2(handleExitIPGet))
	mux.HandleFunc("/api/m/exitip-set", tapi2(handleExitIPSet))
	mux.HandleFunc("/api/m/port-set", tapi2(handlePortSet))
	mux.HandleFunc("/api/m/sec-get", tapi2(handleSecGet))
	mux.HandleFunc("/api/m/sec-fix", tapi2(handleSecFix))
}
