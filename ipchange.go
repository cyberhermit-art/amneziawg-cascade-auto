// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

// «Сервер сменил IP»: хостер поменял адрес одного из серверов (или его пришлось
// сменить из-за блокировки), а содержимое сервера осталось прежним.
//
// Ключи AmneziaWG к адресу не привязаны — адрес записан только в Endpoint у тех,
// кто подключается к серверу. Поэтому:
//   - сменился RU: на сервере прописываем новый адрес (AWG_ENDPOINT) и
//     перегенерируем файлы подключения всех пользователей (ключи те же);
//     устройства должны импортировать подключение заново;
//   - сменился EU: на RU меняем адрес в туннеле awg1 и в маршрутизации;
//     пользователей это не касается.
//
// Что по новому адресу тот же сервер, а не чужой, проверяется криптографически:
// туннель RU → EU устанавливает связь только при совпадающих ключах.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ipReq struct {
	ID      string `json:"id"`
	Target  string `json:"target"` // ru | eu
	NewHost string `json:"newHost"`
	Force   bool   `json:"force"` // продолжить, даже если проверка не подтвердила, что это тот же сервер
}

func parseNewIP(s string) (string, error) {
	s = strings.TrimSpace(s)
	ip := net.ParseIP(s)
	if ip == nil || ip.To4() == nil {
		return "", errors.New("введите новый IPv4-адрес сервера, например 203.0.113.25")
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
		return "", errors.New("это не публичный адрес — нужен внешний IP сервера, который выдал хостер")
	}
	return ip.To4().String(), nil
}

func kvLines(out string) map[string]string {
	kv := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "@@") {
			f := strings.SplitN(strings.TrimPrefix(l, "@@"), " ", 2)
			if len(f) == 2 {
				kv[f[0]] = strings.TrimSpace(f[1])
			} else {
				kv[f[0]] = ""
			}
		}
	}
	return kv
}

func handleIPChange(m *mctx, r ipReq) (any, error) {
	role, err := roleOf(r.Target)
	if err != nil {
		return nil, err
	}
	ip, err := parseNewIP(r.NewHost)
	if err != nil {
		return nil, err
	}
	if role == "RU" {
		if ip == m.p.RU.Host {
			return nil, errors.New("этот адрес уже указан для сервера RU")
		}
		if ip == m.p.EU.Host {
			return nil, errors.New("это адрес сервера EU")
		}
		return ipChangeRU(m, ip, r.Force)
	}
	if ip == m.p.EU.Host {
		return nil, errors.New("этот адрес уже указан для сервера EU")
	}
	if ip == m.p.RU.Host {
		return nil, errors.New("это адрес сервера RU")
	}
	return ipChangeEU(m, ip, r.Force)
}

// ---------- сменился адрес RU ----------

func ipChangeRU(m *mctx, ip string, force bool) (any, error) {
	old := m.p.RU.Host
	np := *m.p
	np.RU.Host = ip
	c := getConn(&np, "RU")

	// 1. Тот ли это сервер: туннель до EU по-прежнему устанавливает связь —
	// значит, у сервера ключ, который знает наш EU.
	out, _, _, err := c.lockedRun(`C=/etc/amnezia/amneziawg
[ -f $C/awg0.conf ] || { echo "@@ERR noawg"; exit 0; }
[ -f $C/awg1.conf ] || echo "@@ERR nocascade"
echo "@@EP $(grep -oP '^AWG1_ENDPOINT="\K[^"]+' /root/awg/awg-routing.sh 2>/dev/null)"
hs(){ awg show awg1 latest-handshakes 2>/dev/null | awk 'NR==1{print $2}'; }
H=$(hs); T=$(date +%s)
if [ -z "$H" ] || [ "$H" = 0 ] || [ $((T-H)) -gt 170 ]; then
  ping -c3 -W2 -I awg1 1.1.1.1 >/dev/null 2>&1; sleep 2; H=$(hs); T=$(date +%s)
fi
echo "@@HS ${H:-0} $T"
echo "@@PEERS $(grep -c '^#_Name = ' $C/awg0.conf)"`, nil, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("не удалось подключиться к %s по SSH: %v. Логин и пароль (или ключ) берутся из профиля", ip, err)
	}
	fp := c.r.Fingerprint()
	kv := kvLines(out)
	if kv["ERR"] == "noawg" {
		return nil, fmt.Errorf("на сервере %s нет AmneziaWG — это не сервер RU этого каскада (ключ SSH сервера: %s)", ip, fp)
	}
	var hs, now int64
	fmt.Sscan(kv["HS"], &hs, &now)
	tunnelOK := hs > 0 && now-hs < 300
	epOK := kv["EP"] == m.p.EU.Host
	if !(tunnelOK && epOK) && !force {
		why := "туннель до сервера EU с этого сервера не устанавливает связь"
		switch {
		case kv["ERR"] == "nocascade":
			why = "на сервере нет туннеля до EU (каскад не настроен)"
		case !epOK && kv["EP"] != "":
			why = "этот сервер настроен на другой сервер EU (" + kv["EP"] + ", а в профиле " + m.p.EU.Host + ")"
		}
		return map[string]any{"needForce": true, "fingerprint": fp,
			"reason": "Не удалось подтвердить, что по адресу " + ip + " тот же сервер RU: " + why + ". Если вы уверены — например, сервер EU сейчас недоступен, — продолжите принудительно."}, nil
	}

	// 2. Новый адрес — в настройки сервера, затем перегенерация файлов всех
	// пользователей (ключи, имена и сроки не меняются).
	out, se, code, err := c.lockedRun(fmt.Sprintf(`A=/root/awg; I=$A/awgsetup_cfg.init; N=%[1]s; C=/etc/amnezia/amneziawg/awg0.conf
mkdir -p $A/backups
(cd / && tar czf $A/backups/before-ip-change-$(date +%%F_%%H-%%M-%%S).tgz root/awg/awgsetup_cfg.init $(ls root/awg/*.conf 2>/dev/null)) 2>/dev/null
touch $I
if grep -qE '^[[:space:]]*(export[[:space:]]+)?AWG_ENDPOINT=' $I; then
  sed -i -E "s|^[[:space:]]*(export[[:space:]]+)?AWG_ENDPOINT=.*|export AWG_ENDPOINT='$N'|" $I
else
  echo "export AWG_ENDPOINT='$N'" >> $I
fi
bash %[2]s --yes --no-color regen >/tmp/awgc-regen.log 2>&1 || { echo "@@REGENFAIL"; tail -5 /tmp/awgc-regen.log; }
for n in $(grep '^#_Name = ' $C | sed 's/^#_Name = //'); do
  grep -qE "^Endpoint[[:space:]]*=[[:space:]]*$N:" $A/$n.conf 2>/dev/null && echo "@@OK $n" || echo "@@BAD $n"
done
true`, ip, manageSh), nil, 15*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("сервер RU (%s): %v", ip, err)
	}
	if code != 0 {
		return nil, fmt.Errorf("сервер RU (%s): %s", ip, tail(se+out, 4))
	}
	var okN, badN []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "@@OK ") {
			okN = append(okN, strings.TrimPrefix(l, "@@OK "))
		} else if strings.HasPrefix(l, "@@BAD ") {
			badN = append(badN, strings.TrimPrefix(l, "@@BAD "))
		}
	}
	if strings.Contains(out, "@@REGENFAIL") && len(okN) == 0 {
		return nil, fmt.Errorf("не удалось перегенерировать файлы подключения: %s", tail(strings.ReplaceAll(out, "@@REGENFAIL", ""), 4))
	}

	// 3. Профиль: новый адрес, папка с копиями и файлами остаётся прежней.
	if np.Dir == "" {
		np.Dir = profileDir(m.p)
	}
	saved, err := saveProfile(np)
	if err != nil {
		return nil, fmt.Errorf("сервер перенастроен, но профиль не сохранился: %v — укажите новый адрес во вкладке «Профиль»", err)
	}
	dropConns(np.ID)
	logLine(fmt.Sprintf("[RU] Смена IP %s → %s: файлов подключения обновлено %d, с ошибкой %d", old, ip, len(okN), len(badN)))

	// 4. Новые файлы подключения — в папку профиля.
	dir := filepath.Join(profileDir(saved), "после смены IP "+time.Now().Format("2006-01-02_15-04"))
	saveErr := ""
	if err := os.MkdirAll(dir, 0o700); err != nil {
		saveErr = err.Error()
	} else {
		nm := &mctx{p: saved}
		for _, n := range okN {
			cf, err := nm.fetchClient(n)
			if err != nil {
				saveErr = err.Error()
				continue
			}
			_ = os.WriteFile(filepath.Join(dir, n+".conf"), []byte(cf.Conf), 0o600)
			if cf.VpnURI != "" {
				_ = os.WriteFile(filepath.Join(dir, n+".vpnuri.txt"), []byte(cf.VpnURI), 0o600)
			}
			if b, err := base64.StdEncoding.DecodeString(cf.PNG); err == nil && len(b) > 0 {
				_ = os.WriteFile(filepath.Join(dir, n+".png"), b, 0o600)
			}
		}
	}
	return map[string]any{"ok": true, "role": "RU", "old": old, "new": ip, "fingerprint": fp, "forced": force && !(tunnelOK && epOK),
		"clients": okN, "bad": badN, "folder": dir, "saveError": saveErr}, nil
}

// ---------- сменился адрес EU ----------

func ipChangeEU(m *mctx, ip string, force bool) (any, error) {
	old := m.p.EU.Host
	keep := "0"
	if force {
		keep = "1"
	}
	// Всё делается на сервере RU: адрес в туннеле awg1 и в маршрутизации, затем
	// проверка связи. Связь есть — по новому адресу наш EU (ключи совпали);
	// нет — возвращаем прежние настройки (если не попросили оставить).
	out, se, code, err := m.ruCmd(fmt.Sprintf(`set -u
C=/etc/amnezia/amneziawg/awg1.conf; R=/root/awg/awg-routing.sh; H=/root/ru_host.conf; N=%[1]s; KEEP=%[2]s
[ -f $C ] && [ -f $R ] || { echo "@@ERR nocascade"; exit 0; }
cur=$(sed -n -E 's/^Endpoint[[:space:]]*=[[:space:]]*//p' $C | head -1); oh=${cur%%:*}; port=${cur##*:}
echo "@@CUR $cur"
for f in $C $R $H; do [ -f $f ] && cp -p $f $f.bak-ipchange; done
sed -i -E "s|^(Endpoint[[:space:]]*=[[:space:]]*).*|\1$N:$port|" $C
sed -i -E "s|^AWG1_ENDPOINT=\"[^\"]*\"|AWG1_ENDPOINT=\"$N\"|" $R
[ -f $H ] && sed -i -E "s|^(Endpoint[[:space:]]*=[[:space:]]*).*|\1$N:$port|" $H
ip route del "$oh" 2>/dev/null
systemctl restart awg-quick@awg1; systemctl restart awg-routing
ok=0
for i in $(seq 1 15); do
  ping -c1 -W1 -I awg1 1.1.1.1 >/dev/null 2>&1
  hs=$(awg show awg1 latest-handshakes 2>/dev/null | awk 'NR==1{print $2}'); t=$(date +%%s)
  if [ -n "$hs" ] && [ "$hs" != 0 ] && [ $((t-hs)) -lt 90 ]; then ok=1; break; fi
  sleep 2
done
if [ $ok = 1 ]; then echo "@@OK"; exit 0; fi
if [ "$KEEP" = 1 ]; then echo "@@NOHS"; exit 0; fi
for f in $C $R $H; do [ -f $f.bak-ipchange ] && cp -p $f.bak-ipchange $f; done
ip route del "$N" 2>/dev/null
systemctl restart awg-quick@awg1; systemctl restart awg-routing
echo "@@ROLLBACK"`, ip, keep), nil, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("сервер RU: %s", tail(se+out, 4))
	}
	kv := kvLines(out)
	if kv["ERR"] == "nocascade" {
		return nil, errors.New("на сервере RU нет настроенного каскада (туннеля awg1)")
	}
	if _, rb := kv["ROLLBACK"]; rb {
		return map[string]any{"needForce": true,
			"reason": "По адресу " + ip + " сервер EU каскада не ответил: туннель не установил связь, поэтому прежние настройки возвращены. Проверьте адрес и что сервер EU включён. Если сервер ещё загружается, можно сохранить новый адрес принудительно — туннель заработает, когда сервер станет доступен."}, nil
	}
	_, connected := kv["OK"]

	// Профиль: новый адрес EU.
	np := *m.p
	np.EU.Host = ip
	if np.Dir == "" {
		np.Dir = profileDir(m.p)
	}
	saved, err := saveProfile(np)
	if err != nil {
		return nil, fmt.Errorf("туннель перенастроен, но профиль не сохранился: %v — укажите новый адрес во вкладке «Профиль»", err)
	}
	dropConns(np.ID)

	// Необязательно: на самом EU прописать новый адрес, чтобы его собственные
	// файлы (клиент ru_host) тоже были с новым адресом.
	euNote := ""
	if saved.EU.Password != "" || saved.EU.Key != "" {
		c := getConn(saved, "EU")
		_, _, code, err := c.lockedRun(fmt.Sprintf(`I=/root/awg/awgsetup_cfg.init; N=%[1]s
[ -f /etc/amnezia/amneziawg/awg0.conf ] || exit 3
touch $I
if grep -qE '^[[:space:]]*(export[[:space:]]+)?AWG_ENDPOINT=' $I; then sed -i -E "s|^[[:space:]]*(export[[:space:]]+)?AWG_ENDPOINT=.*|export AWG_ENDPOINT='$N'|" $I; else echo "export AWG_ENDPOINT='$N'" >> $I; fi
grep -qxF '#_Name = ru_host' /etc/amnezia/amneziawg/awg0.conf && bash %[2]s --yes --no-color regen ru_host >/dev/null 2>&1
true`, ip, manageSh), nil, 3*time.Minute)
		if err != nil || code != 0 {
			euNote = "На самом сервере EU адрес не обновлён (нет связи по SSH) — на работу каскада это не влияет."
		}
	}
	logLine(fmt.Sprintf("[EU] Смена IP %s → %s: туннель %s", old, ip, map[bool]string{true: "работает", false: "пока не установил связь (сохранено принудительно)"}[connected]))
	return map[string]any{"ok": true, "role": "EU", "old": old, "new": ip, "connected": connected, "note": euNote}, nil
}

// logLine пишет строку в журнал log.txt (действия вкладок управления).
func logLine(s string) {
	if logFile == nil {
		return
	}
	logMu.Lock()
	fmt.Fprintf(logFile, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), s)
	logMu.Unlock()
}

func ipapi(f func(m *mctx, r ipReq) (any, error)) func(w http.ResponseWriter, r *http.Request) {
	return api(func(w http.ResponseWriter, r *http.Request) {
		var ir ipReq
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&ir); err != nil {
			writeJSON(w, map[string]string{"error": "неверный запрос"})
			return
		}
		p, err := getProfile(ir.ID)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		res, err := f(&mctx{p: p}, ir)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, res)
	})
}

func registerIPChange(mux *http.ServeMux) {
	mux.HandleFunc("/api/m/ip-change", ipapi(handleIPChange))
	// открыть папку внутри папки профиля (например, с новыми файлами после смены IP)
	mux.HandleFunc("/api/m/open-folder", api(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ ID, Dir string }
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req) != nil {
			writeJSON(w, map[string]string{"error": "неверный запрос"})
			return
		}
		p, err := getProfile(req.ID)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		base := filepath.Clean(profileDir(p))
		d := filepath.Clean(req.Dir)
		if d != base && !strings.HasPrefix(d, base+string(filepath.Separator)) {
			writeJSON(w, map[string]string{"error": "папка вне папки профиля"})
			return
		}
		openFolder(d)
		writeJSON(w, map[string]any{"ok": true})
	}))
}
