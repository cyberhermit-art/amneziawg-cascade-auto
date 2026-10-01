// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed assets/awgc-stats.sh
var statsScript []byte

const manageSh = "/root/awg/manage_amneziawg.sh"

// ---------- пул подключений к серверам профилей ----------

type conn struct {
	mu     sync.Mutex // одна операция на сервер за раз
	r      *Remote
	sig    string // по чему понять, что данные входа поменялись
	prepOK bool   // сборщик статистики уже проверен в этой сессии
}

var (
	poolMu sync.Mutex
	pool   = map[string]*conn{} // ключ: id профиля + роль
)

func sigOf(s ServerCfg) string {
	return fmt.Sprintf("%s|%d|%s|%d|%d|%d", s.Host, s.Port, s.User, len(s.Password), len(s.Key), len(s.KeyPass)) + s.Password + s.Key
}

func getConn(p *Profile, role string) *conn {
	cfg := p.RU
	if role == "EU" {
		cfg = p.EU
	}
	cfg.Role = role
	if cfg.Port == 0 {
		cfg.Port = 22
	}
	key := p.ID + "/" + role
	poolMu.Lock()
	defer poolMu.Unlock()
	c := pool[key]
	if c == nil || c.sig != sigOf(cfg) {
		if c != nil && c.r != nil {
			c.r.Close()
		}
		c = &conn{r: NewRemote(cfg, func(string) {}), sig: sigOf(cfg)}
		pool[key] = c
	}
	return c
}

func dropConns(id string) {
	poolMu.Lock()
	defer poolMu.Unlock()
	for k, c := range pool {
		if strings.HasPrefix(k, id+"/") {
			if c.r != nil {
				c.r.Close()
			}
			delete(pool, k)
		}
	}
}

// ensure — подключиться, если подключения нет или оно умерло.
func (c *conn) ensure() error {
	if c.r.Alive() {
		return nil
	}
	return c.r.Connect()
}

// run выполняет команду; при обрыве связи один раз переподключается и повторяет.
func (c *conn) run(cmd string, stdin []byte, timeout time.Duration) (string, string, int, error) {
	if err := c.ensure(); err != nil {
		return "", "", -1, err
	}
	so, se, code, err := c.r.Output2(cmd, stdin, timeout)
	if errors.Is(err, errConnLost) {
		if err2 := c.r.Connect(); err2 != nil {
			return "", "", -1, err2
		}
		so, se, code, err = c.r.Output2(cmd, stdin, timeout)
	}
	if errors.Is(err, errConnLost) {
		err = errors.New("связь с сервером прервалась, попробуйте ещё раз")
	}
	return so, se, code, err
}

// ---------- общий обработчик ----------

type mreq struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	NewName string `json:"newName"`
	Expires string `json:"expires"`
	Days    int    `json:"days"`
	Backup  string `json:"backup"`
	Target  string `json:"target"`
}

type mctx struct {
	p   *Profile
	req mreq
}

// mapi: разбирает запрос, находит профиль, ловит ошибки и отдаёт JSON.
func mapi(f func(m *mctx) (any, error)) http.HandlerFunc {
	return api(func(w http.ResponseWriter, r *http.Request) {
		var req mreq
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, map[string]string{"error": "неверный запрос"})
			return
		}
		p, err := getProfile(req.ID)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		res, err := f(&mctx{p: p, req: req})
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, res)
	})
}

var clientNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

func checkName(n string) error {
	if !clientNameRe.MatchString(n) {
		return errors.New("имя: только латинские буквы, цифры, _ и - (до 32 символов)")
	}
	return nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(clean(s))
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ruCmd выполняет команду на сервере RU под замком этого сервера.
func (m *mctx) ruCmd(cmd string, stdin []byte, timeout time.Duration) (string, string, int, error) {
	c := getConn(m.p, "RU")
	c.mu.Lock()
	defer c.mu.Unlock()
	so, se, code, err := c.run(cmd, stdin, timeout)
	if err != nil {
		return so, se, code, fmt.Errorf("сервер RU (%s): %v", m.p.RU.Host, err)
	}
	return so, se, code, nil
}

// prepareRU один раз за сессию ставит на сервер сборщик статистики.
func (m *mctx) prepareRU() {
	c := getConn(m.p, "RU")
	c.mu.Lock()
	done := c.prepOK
	c.mu.Unlock()
	if done {
		return
	}
	cmd := `umask 077; mkdir -p /root/awg; cat > /root/awg/awgc-stats.sh.new && chmod 700 /root/awg/awgc-stats.sh.new && mv -f /root/awg/awgc-stats.sh.new /root/awg/awgc-stats.sh
echo '*/5 * * * * root /root/awg/awgc-stats.sh >/dev/null 2>&1' > /etc/cron.d/awgc-stats; chmod 644 /etc/cron.d/awgc-stats
/root/awg/awgc-stats.sh >/dev/null 2>&1 || true`
	if _, _, code, err := m.ruCmd(cmd, statsScript, time.Minute); err == nil && code == 0 {
		c.mu.Lock()
		c.prepOK = true
		c.mu.Unlock()
	}
}

// ---------- профили ----------

func handleProfiles(w http.ResponseWriter, r *http.Request) {
	ps, last, err := listProfiles()
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	out := []publicProfile{}
	for _, p := range ps {
		out = append(out, toPublic(p))
	}
	writeJSON(w, map[string]any{"profiles": out, "last": last})
}

type profileForm struct {
	ID   string    `json:"id"`
	Name string    `json:"name"`
	RU   ServerCfg `json:"ru"`
	EU   ServerCfg `json:"eu"`
	Test bool      `json:"test"`
}

func handleProfileSave(w http.ResponseWriter, r *http.Request) {
	var f profileForm
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&f); err != nil {
		writeJSON(w, map[string]string{"error": "неверные данные"})
		return
	}
	p := Profile{ID: f.ID, Name: f.Name, RU: f.RU, EU: f.EU}
	for _, s := range []*ServerCfg{&p.RU, &p.EU} {
		s.Host = strings.TrimSpace(s.Host)
		if s.Port == 0 {
			s.Port = 22
		}
		if s.User == "" {
			s.User = "root"
		}
	}
	if p.RU.Host == "" {
		writeJSON(w, map[string]string{"error": "укажите IP сервера RU"})
		return
	}
	if f.ID == "" && p.RU.Password == "" && p.RU.Key == "" {
		writeJSON(w, map[string]string{"error": "укажите пароль или ключ для сервера RU"})
		return
	}
	if f.ID != "" {
		if old, err := getProfile(f.ID); err == nil {
			keepSecrets(&p.RU, old.RU)
			keepSecrets(&p.EU, old.EU)
		}
	}
	if f.Test {
		// Проверяем, что к серверу RU можно подключиться и на нём стоит AmneziaWG.
		rm := NewRemote(ServerCfg{Role: "RU", Host: p.RU.Host, Port: p.RU.Port, User: p.RU.User, Password: p.RU.Password, Key: p.RU.Key, KeyPass: p.RU.KeyPass}, func(string) {})
		if err := rm.Connect(); err != nil {
			writeJSON(w, map[string]string{"error": "сервер RU: " + err.Error()})
			return
		}
		out, _, err := rm.Output(`[ -f `+manageSh+` ] && [ -f /etc/amnezia/amneziawg/awg0.conf ] && echo OK`, nil, 30*time.Second)
		rm.Close()
		if err != nil || !strings.Contains(out, "OK") {
			writeJSON(w, map[string]string{"error": "к серверу RU подключились, но AmneziaWG на нём не найден"})
			return
		}
	}
	saved, err := saveProfile(p)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	dropConns(saved.ID)
	writeJSON(w, map[string]any{"ok": true, "profile": toPublic(*saved)})
}

func handleProfileDelete(w http.ResponseWriter, r *http.Request) {
	var req mreq
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req)
	dropConns(req.ID)
	if err := deleteProfile(req.ID); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---------- состояние ----------

type statusResp struct {
	RUReachable bool     `json:"ruReachable"`
	RUError     string   `json:"ruError,omitempty"`
	PingMs      int      `json:"pingMs"`
	EUReachable bool     `json:"euReachable"`
	EUPingMs    int      `json:"euPingMs"`
	Uptime      int64    `json:"uptime"`
	Load        string   `json:"load"`
	MemTotal    int      `json:"memTotal"`
	MemUsed     int      `json:"memUsed"`
	Disk        string   `json:"disk"`
	OS          string   `json:"os"`
	AWG0        string   `json:"awg0"`
	AWG1        string   `json:"awg1"`
	Routing     string   `json:"routing"`
	TunnelAge   int64    `json:"tunnelAge"` // секунд с последнего рукопожатия с EU, -1 = не было
	RUNets      int      `json:"ruNets"`
	Peers       int      `json:"peers"`
	Endpoint    string   `json:"endpoint"`
	Reboot      bool     `json:"rebootRequired"`
	Problems    []string `json:"problems"`
}

func tcpPing(host string, port int) (bool, int) {
	if port == 0 {
		port = 22
	}
	t := time.Now()
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 6*time.Second)
	if err != nil {
		return false, 0
	}
	c.Close()
	return true, int(time.Since(t).Milliseconds())
}

func mStatus(m *mctx) (any, error) {
	st := &statusResp{TunnelAge: -1, Problems: []string{}}
	var wg sync.WaitGroup
	if m.p.EU.Host != "" {
		wg.Add(1)
		go func() { defer wg.Done(); st.EUReachable, st.EUPingMs = tcpPing(m.p.EU.Host, m.p.EU.Port) }()
	}
	t0 := time.Now()
	out, _, _, err := m.ruCmd(`echo "@@UP $(cut -d' ' -f1 /proc/uptime)"
echo "@@LOAD $(cut -d' ' -f1-3 /proc/loadavg)"
echo "@@MEM $(free -m | awk '/^Mem:/{print $2" "$3}')"
echo "@@DISK $(df -h / | awk 'NR==2{print $3" из "$2" ("$5")"}')"
. /etc/os-release 2>/dev/null; echo "@@OS $PRETTY_NAME"
echo "@@AWG0 $(systemctl is-active awg-quick@awg0 2>/dev/null)"
echo "@@AWG1 $(systemctl is-active awg-quick@awg1 2>/dev/null)"
echo "@@ROUT $(systemctl is-active awg-routing 2>/dev/null)"
echo "@@HS $(awg show awg1 latest-handshakes 2>/dev/null | awk 'NR==1{print $2}')"
echo "@@NOW $(date +%s)"
echo "@@NETS $(ipset list ru 2>/dev/null | awk -F': ' '/Number of entries/{print $2}')"
echo "@@PEERS $(grep -c '^\[Peer\]' /etc/amnezia/amneziawg/awg0.conf 2>/dev/null)"
echo "@@EP $(grep -oP '^AWG1_ENDPOINT="\K[^"]+' /root/awg/awg-routing.sh 2>/dev/null)"
echo "@@RB $([ -f /run/reboot-required ] && echo 1)"`, nil, 60*time.Second)
	st.PingMs = int(time.Since(t0).Milliseconds())
	wg.Wait()
	if err != nil {
		st.RUError = err.Error()
		st.Problems = append(st.Problems, "Нет связи с сервером RU: "+err.Error())
		return st, nil
	}
	st.RUReachable = true
	kv := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "@@") {
			f := strings.SplitN(strings.TrimPrefix(l, "@@"), " ", 2)
			if len(f) == 2 {
				kv[f[0]] = strings.TrimSpace(f[1])
			} else {
				kv[f[0]] = ""
			}
		}
	}
	up, _ := strconv.ParseFloat(kv["UP"], 64)
	st.Uptime = int64(up)
	st.Load, st.Disk, st.OS = kv["LOAD"], kv["DISK"], kv["OS"]
	if mf := strings.Fields(kv["MEM"]); len(mf) == 2 {
		st.MemTotal, _ = strconv.Atoi(mf[0])
		st.MemUsed, _ = strconv.Atoi(mf[1])
	}
	st.AWG0, st.AWG1, st.Routing = kv["AWG0"], kv["AWG1"], kv["ROUT"]
	hs, _ := strconv.ParseInt(kv["HS"], 10, 64)
	now, _ := strconv.ParseInt(kv["NOW"], 10, 64)
	if hs > 0 && now > 0 {
		st.TunnelAge = now - hs
	}
	st.RUNets, _ = strconv.Atoi(kv["NETS"])
	st.Peers, _ = strconv.Atoi(kv["PEERS"])
	st.Endpoint = kv["EP"]
	st.Reboot = kv["RB"] == "1"

	if st.AWG0 != "active" {
		st.Problems = append(st.Problems, "VPN-сервер на RU не запущен (awg-quick@awg0: "+st.AWG0+")")
	}
	if st.AWG1 != "active" {
		st.Problems = append(st.Problems, "Туннель RU → EU не запущен (awg-quick@awg1: "+st.AWG1+")")
	} else if st.TunnelAge < 0 || st.TunnelAge > 300 {
		st.Problems = append(st.Problems, "Туннель RU → EU не отвечает: сервер EU недоступен или его VPN остановлен")
	}
	if st.Routing != "active" {
		st.Problems = append(st.Problems, "Раздельная маршрутизация не применена (awg-routing: "+st.Routing+")")
	}
	if st.Routing == "active" && st.RUNets < 1000 {
		st.Problems = append(st.Problems, "Список российских сетей пуст — весь трафик идёт через EU")
	}
	if m.p.EU.Host != "" && !st.EUReachable {
		st.Problems = append(st.Problems, "Сервер EU не отвечает по SSH с этого компьютера")
	}
	go m.prepareRU()
	return st, nil
}

// ---------- клиенты ----------

type clientRow struct {
	Name       string `json:"name"`
	IP         string `json:"ip"`
	Status     string `json:"status"`
	StatusCode string `json:"statusCode"`
	Rx         int64  `json:"rx"` // получено сервером от клиента (отдача клиента)
	Tx         int64  `json:"tx"` // отправлено клиенту (загрузка клиента)
	Handshake  int64  `json:"handshake"`
	ExpiresAt  int64  `json:"expiresAt,omitempty"`
	TodayRx    int64  `json:"todayRx"`
	TodayTx    int64  `json:"todayTx"`
}

// jsonAfter достаёт JSON, напечатанный после маркера.
func jsonAfter(out, marker string, v any) error {
	i := strings.Index(out, marker)
	if i < 0 {
		return errors.New("нет данных")
	}
	s := out[i+len(marker):]
	if j := strings.Index(s, "@@"); j >= 0 {
		s = s[:j]
	}
	// Строки журнала тоже могут начинаться с «[» — берём первую, что разбирается как JSON.
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[") || strings.HasPrefix(l, "{") {
			if json.Unmarshal([]byte(l), v) == nil {
				return nil
			}
		}
	}
	return errors.New("нет JSON в ответе")
}

func mClients(m *mctx) (any, error) {
	m.prepareRU()
	out, se, _, err := m.ruCmd(`M=`+manageSh+`
echo "@@LIST"; bash $M --json --no-color list 2>/dev/null
echo "@@STATS"; bash $M --json --no-color stats 2>/dev/null
echo "@@TODAY"; awk -F'\t' -v d="$(date +%F)" '$1==d{print $2"\t"$3"\t"$4}' /root/awg/awgc-stats/daily.tsv 2>/dev/null
echo "@@NOW $(date +%s)"`, nil, 2*time.Minute)
	if err != nil {
		return nil, err
	}
	var list []struct {
		Name       string `json:"name"`
		IP         string `json:"ip"`
		Status     string `json:"status"`
		StatusCode string `json:"status_code"`
		ExpiresAt  *int64 `json:"expires_at"`
	}
	if err := jsonAfter(out, "@@LIST", &list); err != nil {
		return nil, fmt.Errorf("не удалось получить список клиентов: %s", tail(se, 3))
	}
	var stats []struct {
		Name       string `json:"name"`
		Rx         int64  `json:"rx"`
		Tx         int64  `json:"tx"`
		Handshake  int64  `json:"last_handshake"`
		Status     string `json:"status"`
		StatusCode string `json:"status_code"`
	}
	_ = jsonAfter(out, "@@STATS", &stats)
	byName := map[string]*clientRow{}
	var rows []*clientRow
	for _, c := range list {
		r := &clientRow{Name: c.Name, IP: c.IP, Status: c.Status, StatusCode: c.StatusCode}
		if c.ExpiresAt != nil {
			r.ExpiresAt = *c.ExpiresAt
		}
		byName[c.Name] = r
		rows = append(rows, r)
	}
	for _, s := range stats {
		if r := byName[s.Name]; r != nil {
			r.Rx, r.Tx, r.Handshake = s.Rx, s.Tx, s.Handshake
			if s.StatusCode != "" {
				r.Status, r.StatusCode = s.Status, s.StatusCode
			}
		}
	}
	if i := strings.Index(out, "@@TODAY"); i >= 0 {
		for _, l := range strings.Split(out[i+7:], "\n") {
			f := strings.Split(strings.TrimSpace(l), "\t")
			if len(f) == 3 {
				if r := byName[f[0]]; r != nil {
					r.TodayRx, _ = strconv.ParseInt(f[1], 10, 64)
					r.TodayTx, _ = strconv.ParseInt(f[2], 10, 64)
				}
			}
		}
	}
	var now int64
	if i := strings.Index(out, "@@NOW "); i >= 0 {
		now, _ = strconv.ParseInt(strings.TrimSpace(strings.SplitN(out[i+6:], "\n", 2)[0]), 10, 64)
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].Name < rows[b].Name })
	return map[string]any{"clients": rows, "now": now}, nil
}

func mClient(m *mctx) (any, error) {
	if err := checkName(m.req.Name); err != nil {
		return nil, err
	}
	return m.fetchClient(m.req.Name)
}

func (m *mctx) fetchClient(n string) (*ClientFile, error) {
	out, _, code, err := m.ruCmd(fmt.Sprintf(`cd /root/awg && [ -f %[1]s.conf ] || exit 3
echo "@@CONF"; base64 -w0 %[1]s.conf; echo
[ -f %[1]s.vpnuri ] && { echo "@@URI"; base64 -w0 %[1]s.vpnuri; echo; }
[ -f %[1]s.png ] && { echo "@@PNG"; base64 -w0 %[1]s.png; echo; }
true`, n), nil, time.Minute)
	if err != nil {
		return nil, err
	}
	if code == 3 {
		return nil, fmt.Errorf("файл клиента %s не найден на сервере", n)
	}
	cf := &ClientFile{Name: n}
	lines := strings.Split(out, "\n")
	for i := 0; i+1 < len(lines); i++ {
		v := strings.TrimSpace(lines[i+1])
		switch strings.TrimSpace(lines[i]) {
		case "@@CONF":
			b, _ := base64.StdEncoding.DecodeString(v)
			cf.Conf = string(b)
		case "@@URI":
			b, _ := base64.StdEncoding.DecodeString(v)
			cf.VpnURI = strings.TrimSpace(string(b))
		case "@@PNG":
			cf.PNG = v
		}
	}
	if cf.Conf == "" {
		return nil, errors.New("не удалось прочитать файл клиента")
	}
	return cf, nil
}

var expiresRe = regexp.MustCompile(`^[0-9]{1,4}[hdw]$`)

func mAdd(m *mctx) (any, error) {
	n := m.req.Name
	if err := checkName(n); err != nil {
		return nil, err
	}
	opt := ""
	if m.req.Expires != "" {
		if !expiresRe.MatchString(m.req.Expires) {
			return nil, errors.New("неверный срок действия")
		}
		opt = " --expires=" + m.req.Expires
	}
	out, se, code, err := m.ruCmd(fmt.Sprintf(`grep -qxF '#_Name = %[1]s' /etc/amnezia/amneziawg/awg0.conf && { echo "Клиент с таким именем уже есть" >&2; exit 4; }
bash %[2]s --yes --no-color%[3]s add %[1]s`, n, manageSh, opt), nil, 3*time.Minute)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("не удалось добавить клиента: %s", tail(se+out, 4))
	}
	return m.fetchClient(n)
}

func mRemove(m *mctx) (any, error) {
	n := m.req.Name
	if err := checkName(n); err != nil {
		return nil, err
	}
	out, se, code, err := m.ruCmd(fmt.Sprintf(`bash %s --yes --no-color remove %s`, manageSh, n), nil, 3*time.Minute)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("не удалось удалить клиента: %s", tail(se+out, 4))
	}
	return map[string]any{"ok": true}, nil
}

// mRename меняет имя без смены ключа: устройство клиента продолжит работать.
func mRename(m *mctx) (any, error) {
	o, n := m.req.Name, m.req.NewName
	if err := checkName(o); err != nil {
		return nil, err
	}
	if err := checkName(n); err != nil {
		return nil, err
	}
	if o == n {
		return nil, errors.New("новое имя совпадает со старым")
	}
	script := fmt.Sprintf(`set -e
O=%[1]s; N=%[2]s; A=/root/awg; C=/etc/amnezia/amneziawg/awg0.conf
exec 8>"$A/.awgc-rename.lock"; flock -w 60 8
grep -qxF "#_Name = $O" "$C" || { echo "Клиент $O не найден" >&2; exit 4; }
grep -qxF "#_Name = $N" "$C" && { echo "Имя $N уже занято" >&2; exit 4; }
[ -e "$A/$N.conf" ] && { echo "Файл $N.conf уже существует" >&2; exit 4; }
cp -p "$C" "$C.awgc-rename.bak"
sed -i "s/^#_Name = $O\$/#_Name = $N/" "$C"
for e in private public; do [ -f "$A/keys/$O.$e" ] && mv -f "$A/keys/$O.$e" "$A/keys/$N.$e"; done
[ -f "$A/expiry/$O" ] && mv -f "$A/expiry/$O" "$A/expiry/$N"
[ -f "$A/$O.conf" ] && mv -f "$A/$O.conf" "$A/$N.conf"
rm -f "$A/$O.png" "$A/$O.vpnuri" "$A/$O.vpnuri.png"
[ -f "$A/awgc-stats/daily.tsv" ] && awk -F'\t' -v OFS='\t' -v o="$O" -v n="$N" '$2==o{$2=n}1' "$A/awgc-stats/daily.tsv" > "$A/awgc-stats/daily.tmp" && mv -f "$A/awgc-stats/daily.tmp" "$A/awgc-stats/daily.tsv"
if ! bash %[3]s --no-color regen "$N"; then
  echo "Перегенерация не удалась — возвращаю прежнее имя" >&2
  cp -p "$C.awgc-rename.bak" "$C"
  for e in private public; do [ -f "$A/keys/$N.$e" ] && mv -f "$A/keys/$N.$e" "$A/keys/$O.$e"; done
  [ -f "$A/$N.conf" ] && mv -f "$A/$N.conf" "$A/$O.conf"
  [ -f "$A/expiry/$N" ] && mv -f "$A/expiry/$N" "$A/expiry/$O"
  bash %[3]s --no-color regen "$O" || true
  exit 5
fi
rm -f "$C.awgc-rename.bak"`, o, n, manageSh)
	out, se, code, err := m.ruCmd(script, nil, 3*time.Minute)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("не удалось переименовать: %s", tail(se+out, 4))
	}
	return m.fetchClient(n)
}

// mReissue выпускает новый ключ: старое подключение на устройстве перестаёт работать.
func mReissue(m *mctx) (any, error) {
	n := m.req.Name
	if err := checkName(n); err != nil {
		return nil, err
	}
	out, se, code, err := m.ruCmd(fmt.Sprintf(`bash %[1]s --yes --no-color remove %[2]s || exit 6
bash %[1]s --yes --no-color add %[2]s || exit 7`, manageSh, n), nil, 4*time.Minute)
	if err != nil {
		return nil, err
	}
	switch code {
	case 0:
	case 6:
		return nil, fmt.Errorf("не удалось удалить старый ключ: %s", tail(se+out, 4))
	case 7:
		return nil, fmt.Errorf("старый ключ удалён, но новый выпустить не удалось — добавьте клиента %s заново: %s", n, tail(se+out, 4))
	default:
		return nil, fmt.Errorf("ошибка: %s", tail(se+out, 4))
	}
	return m.fetchClient(n)
}

// ---------- статистика ----------

type dayRow struct {
	Date string `json:"date"`
	Name string `json:"name"`
	Rx   int64  `json:"rx"`
	Tx   int64  `json:"tx"`
}

func mStats(m *mctx) (any, error) {
	m.prepareRU()
	days := m.req.Days
	if days <= 0 || days > 400 {
		days = 30
	}
	out, _, _, err := m.ruCmd(fmt.Sprintf(`F=/root/awg/awgc-stats/daily.tsv
echo "@@SINCE $(head -1 $F 2>/dev/null | cut -f1)"
echo "@@TODAY $(date +%%F)"
awk -F'\t' -v c="$(date -d '-%d days' +%%F)" '$1>c' $F 2>/dev/null`, days-1), nil, time.Minute)
	if err != nil {
		return nil, err
	}
	rows := []dayRow{}
	since, today := "", ""
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		switch {
		case strings.HasPrefix(l, "@@SINCE"):
			since = strings.TrimSpace(strings.TrimPrefix(l, "@@SINCE"))
		case strings.HasPrefix(l, "@@TODAY"):
			today = strings.TrimSpace(strings.TrimPrefix(l, "@@TODAY"))
		default:
			f := strings.Split(l, "\t")
			if len(f) == 4 {
				rx, _ := strconv.ParseInt(f[2], 10, 64)
				tx, _ := strconv.ParseInt(f[3], 10, 64)
				rows = append(rows, dayRow{f[0], f[1], rx, tx})
			}
		}
	}
	return map[string]any{"rows": rows, "since": since, "today": today, "days": days}, nil
}

// ---------- обслуживание ----------

func mRestartVPN(m *mctx) (any, error) {
	out, se, code, err := m.ruCmd(`systemctl restart awg-quick@awg0 && { systemctl restart awg-quick@awg1 2>/dev/null; systemctl restart awg-routing 2>/dev/null; true; }`, nil, 3*time.Minute)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("перезапуск не удался: %s", tail(se+out, 4))
	}
	return map[string]any{"ok": true}, nil
}

func mReboot(m *mctx) (any, error) {
	_, se, code, err := m.ruCmd(`systemd-run --quiet --on-active=2 /bin/systemctl reboot`, nil, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("не удалось перезагрузить: %s", tail(se, 3))
	}
	dropConns(m.p.ID)
	return map[string]any{"ok": true}, nil
}

// ---------- резервные копии ----------

const backupList = `P=""; for f in etc/amnezia/amneziawg root/awg etc/systemd/system/awg-routing.service etc/cron.d/awg-routing-refresh etc/cron.d/awgc-stats; do [ -e "/$f" ] && P="$P $f"; done
[ -n "$P" ] || { echo "нечего сохранять" >&2; exit 3; }
tar czf - -C / --exclude=root/awg/backups --exclude='root/awg/*.lock' --exclude='root/awg/.*.lock' $P`

type backupInfo struct {
	Name    string   `json:"name"`
	Created string   `json:"created"`
	Files   []string `json:"files"`
	Size    int64    `json:"size"`
	Note    string   `json:"note,omitempty"`
}

func backupsDir(p *Profile) string { return filepath.Join(profileDir(p), "backups") }

func mBackup(m *mctx) (any, error) {
	name := time.Now().Format("2006-01-02_15-04-05")
	dir := filepath.Join(backupsDir(m.p), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info := backupInfo{Name: name, Created: time.Now().Format("2006-01-02 15:04")}
	so, se, code, err := m.ruCmd(backupList, nil, 3*time.Minute)
	if err != nil || code != 0 || len(so) < 100 {
		os.RemoveAll(dir)
		if err == nil {
			err = fmt.Errorf("архив не создан: %s", tail(se, 3))
		}
		return nil, fmt.Errorf("резервная копия RU не создана: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ru.tar.gz"), []byte(so), 0o600); err != nil {
		return nil, err
	}
	info.Files = append(info.Files, "ru.tar.gz")
	// EU — по возможности: там живёт подключение ru_host, без него каскад не восстановить.
	if m.p.EU.Host != "" && (m.p.EU.Password != "" || m.p.EU.Key != "") {
		c := getConn(m.p, "EU")
		c.mu.Lock()
		so, se, code, err := c.run(backupList, nil, 3*time.Minute)
		c.mu.Unlock()
		if err == nil && code == 0 && len(so) > 100 {
			_ = os.WriteFile(filepath.Join(dir, "eu.tar.gz"), []byte(so), 0o600)
			info.Files = append(info.Files, "eu.tar.gz")
		} else {
			if err == nil {
				err = errors.New(tail(se, 2))
			}
			info.Note = "Сервер EU не сохранён: " + err.Error()
		}
	} else {
		info.Note = "Сервер EU не сохранён: в профиле нет данных для входа на него"
	}
	b, _ := json.MarshalIndent(info, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "info.json"), b, 0o600)
	info.Size = dirSize(dir)
	return info, nil
}

func dirSize(dir string) int64 {
	var n int64
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		if i, err := e.Info(); err == nil {
			n += i.Size()
		}
	}
	return n
}

func mBackups(m *mctx) (any, error) {
	root := backupsDir(m.p)
	es, _ := os.ReadDir(root)
	out := []backupInfo{}
	for _, e := range es {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		var bi backupInfo
		if b, err := os.ReadFile(filepath.Join(dir, "info.json")); err == nil {
			_ = json.Unmarshal(b, &bi)
		}
		bi.Name = e.Name()
		bi.Files = nil
		for _, f := range []string{"ru.tar.gz", "eu.tar.gz"} {
			if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
				bi.Files = append(bi.Files, f)
			}
		}
		if len(bi.Files) == 0 {
			continue
		}
		if bi.Created == "" {
			bi.Created = e.Name()
		}
		bi.Size = dirSize(dir)
		out = append(out, bi)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name > out[b].Name })
	return map[string]any{"backups": out, "folder": root}, nil
}

var backupNameRe = regexp.MustCompile(`^[0-9_-]{10,25}$`)

const restoreCmd = `set -e
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
cat > "$T/b.tar.gz"
tar tzf "$T/b.tar.gz" >/dev/null
tar xzf "$T/b.tar.gz" -C /
systemctl daemon-reload
systemctl restart awg-quick@awg0
if [ -f /etc/amnezia/amneziawg/awg1.conf ]; then systemctl enable awg-quick@awg1 >/dev/null 2>&1 || true; systemctl restart awg-quick@awg1 || true; fi
if [ -f /etc/systemd/system/awg-routing.service ]; then systemctl enable awg-routing >/dev/null 2>&1 || true; systemctl restart awg-routing || true; fi
echo RESTORED`

func mRestore(m *mctx) (any, error) {
	if !backupNameRe.MatchString(m.req.Backup) {
		return nil, errors.New("неверное имя резервной копии")
	}
	dir := filepath.Join(backupsDir(m.p), m.req.Backup)
	done := []string{}
	for _, role := range []string{"RU", "EU"} {
		if m.req.Target != "both" && !strings.EqualFold(m.req.Target, role) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, strings.ToLower(role)+".tar.gz"))
		if err != nil {
			if m.req.Target == "both" {
				continue
			}
			return nil, fmt.Errorf("в копии нет архива сервера %s", role)
		}
		c := getConn(m.p, role)
		c.mu.Lock()
		out, se, code, err := c.run(restoreCmd, data, 5*time.Minute)
		c.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("сервер %s: %v", role, err)
		}
		if code != 0 || !strings.Contains(out, "RESTORED") {
			return nil, fmt.Errorf("сервер %s: восстановление не удалось: %s", role, tail(se+out, 4))
		}
		done = append(done, role)
	}
	if len(done) == 0 {
		return nil, errors.New("нечего восстанавливать")
	}
	return map[string]any{"ok": true, "restored": done}, nil
}

func mOpenBackups(m *mctx) (any, error) {
	d := backupsDir(m.p)
	_ = os.MkdirAll(d, 0o700)
	openFolder(d)
	return map[string]any{"ok": true}, nil
}

func registerManage(mux *http.ServeMux) {
	mux.HandleFunc("/api/profiles", api(handleProfiles))
	mux.HandleFunc("/api/profile-save", api(handleProfileSave))
	mux.HandleFunc("/api/profile-delete", api(handleProfileDelete))
	mux.HandleFunc("/api/profile-use", api(func(w http.ResponseWriter, r *http.Request) {
		var req mreq
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req)
		setLastProfile(req.ID)
		writeJSON(w, map[string]any{"ok": true})
	}))
	mux.HandleFunc("/api/m/status", mapi(mStatus))
	mux.HandleFunc("/api/m/clients", mapi(mClients))
	mux.HandleFunc("/api/m/client", mapi(mClient))
	mux.HandleFunc("/api/m/add", mapi(mAdd))
	mux.HandleFunc("/api/m/remove", mapi(mRemove))
	mux.HandleFunc("/api/m/rename", mapi(mRename))
	mux.HandleFunc("/api/m/reissue", mapi(mReissue))
	mux.HandleFunc("/api/m/stats", mapi(mStats))
	mux.HandleFunc("/api/m/restart-vpn", mapi(mRestartVPN))
	mux.HandleFunc("/api/m/reboot", mapi(mReboot))
	mux.HandleFunc("/api/m/backup", mapi(mBackup))
	mux.HandleFunc("/api/m/backups", mapi(mBackups))
	mux.HandleFunc("/api/m/restore", mapi(mRestore))
	mux.HandleFunc("/api/m/open-backups", mapi(mOpenBackups))
	registerTasks(mux)
}
