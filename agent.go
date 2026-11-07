// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

// «Эксперименты» (полная версия): агент на серверах каскада — уведомления в
// Telegram, резервные копии по расписанию, проверка репутации адресов и
// лимиты трафика. Программа на компьютере обычно закрыта, поэтому всё это
// работает на самих серверах (assets/awgc-agent.sh, таймер раз в 5 минут),
// а программа только настраивает агент.

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed assets/awgc-agent.sh
var agentScript []byte

const (
	agentPath   = "/root/awg/awgc-agent.sh"
	agentConf   = "/root/awg/awgc-agent.conf"
	limitsFile  = "/root/awg/awgc-limits.txt"
	agentUninst = `systemctl disable --now awgc-agent.timer awgc-agent.service >/dev/null 2>&1; rm -f /etc/systemd/system/awgc-agent.service /etc/systemd/system/awgc-agent.timer; systemctl daemon-reload; rm -rf /root/awg/awgc-agent /root/awg/awgc-agent.conf /root/awg/awgc-agent.sh`
)

type agentReq struct {
	ID      string             `json:"id"`
	Token   string             `json:"token"`
	Chat    string             `json:"chat"`
	Health  bool               `json:"health"`
	Expiry  int                `json:"expiry"` // за сколько дней предупреждать; 0 — не следить
	Limits  bool               `json:"limits"`
	Rep     bool               `json:"rep"`
	Backup  string             `json:"backup"` // off | daily | weekly
	HourUTC int                `json:"hourUtc"`
	LimitGB map[string]float64 `json:"limitGb"`
}

var (
	tokenRe = regexp.MustCompile(`^\d{5,12}:[A-Za-z0-9_-]{30,50}$`)
	chatRe  = regexp.MustCompile(`^(-?\d{3,20}|@[A-Za-z0-9_]{5,32})$`)
)

// agentServers — серверы профиля, на которые ставится агент: RU всегда, EU и
// выход — если в профиле есть данные для входа.
func agentServers(p *Profile) []string {
	out := []string{"RU"}
	if p.EU.Host != "" && (p.EU.Password != "" || p.EU.Key != "") {
		out = append(out, "EU")
	}
	if p.EX.Host != "" && (p.EX.Password != "" || p.EX.Key != "") {
		out = append(out, "EX")
	}
	return out
}

func readKV(out string) map[string]string {
	kv := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if i := strings.Index(l, "="); i > 0 && !strings.HasPrefix(l, "#") {
			v := strings.TrimSpace(l[i+1:])
			v = strings.TrimSuffix(strings.TrimPrefix(v, "'"), "'")
			kv[l[:i]] = strings.ReplaceAll(v, `'\''`, "'")
		}
	}
	return kv
}

func handleAgentGet(m *mctx, _ agentReq) (any, error) {
	out, _, _, err := m.ruCmd(`cat `+agentConf+` 2>/dev/null; echo "@@S"; [ -f `+agentPath+` ] && bash `+agentPath+` status; echo "@@T"; cat /root/awg/awgc-agent/last-error 2>/dev/null | head -c 300`, nil, time.Minute)
	if err != nil {
		return nil, err
	}
	conf, rest, _ := strings.Cut(out, "@@S")
	st, lastErr, _ := strings.Cut(rest, "@@T")
	kv := readKV(conf)
	s := kvLines(st)
	tok := kv["TG_TOKEN"]
	masked := ""
	if len(tok) > 8 {
		masked = tok[:strings.Index(tok+":", ":")] + ":…" + tok[len(tok)-4:]
	}
	exp, _ := strconv.Atoi(kv["A_EXPIRY"])
	hour, _ := strconv.Atoi(kv["HOUR_UTC"])
	last, _ := strconv.ParseInt(s["LAST"], 10, 64)
	now, _ := strconv.ParseInt(s["NOW"], 10, 64)
	age := int64(-1)
	if last > 0 && now > 0 {
		age = now - last
	}
	return map[string]any{
		"configured": kv["TG_TOKEN"] != "", "active": s["ACTIVE"] == "active", "lastRunAge": age,
		"tokenMasked": masked, "chat": kv["TG_CHAT"],
		"health": kv["A_HEALTH"] != "0", "expiry": exp, "limits": kv["A_LIMITS"] != "0", "rep": kv["A_REP"] != "0",
		"backup": orDefault(kv["BACKUP"], "off"), "hourUtc": hour,
		"servers": agentServers(m.p), "lastError": strings.TrimSpace(lastErr),
	}, nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func agentConfText(p *Profile, role string, r agentReq) string {
	b01 := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	exitIP := p.EU.Host
	if p.EX.Host != "" {
		exitIP = p.EX.Host
	}
	lines := []string{
		"# AmneziaWG Cascade: настройки агента (пишет программа)",
		"TG_TOKEN=" + shq(r.Token), "TG_CHAT=" + shq(r.Chat), "NAME=" + shq(p.Name),
		"A_HEALTH=" + b01(r.Health), "A_EXPIRY=" + strconv.Itoa(r.Expiry), "A_LIMITS=" + b01(r.Limits), "A_REP=" + b01(r.Rep),
		"BACKUP=" + r.Backup, "HOUR_UTC=" + strconv.Itoa(r.HourUTC),
	}
	switch role {
	case "RU":
		lines = append(lines, "ROLE=ru", "EXIT_IP="+shq(exitIP), "RU_IP="+shq(p.RU.Host))
	case "EU":
		lines = append(lines, "ROLE=watch", "WATCH_PEER=ru_host")
	case "EX":
		lines = append(lines, "ROLE=watch", "WATCH_PEER=mid_host")
	}
	return strings.Join(lines, "\n") + "\n"
}

const agentUnitsCmd = `cat > /etc/systemd/system/awgc-agent.service <<'EOF'
[Unit]
Description=AmneziaWG Cascade agent (health, Telegram, backups)
After=network-online.target

[Service]
Type=oneshot
ExecStart=/bin/bash /root/awg/awgc-agent.sh run
TimeoutStartSec=10min
EOF
cat > /etc/systemd/system/awgc-agent.timer <<'EOF'
[Unit]
Description=AmneziaWG Cascade agent every 5 minutes

[Timer]
OnBootSec=3min
OnUnitActiveSec=5min
AccuracySec=30s

[Install]
WantedBy=timers.target
EOF
systemctl daemon-reload && systemctl enable --now awgc-agent.timer >/dev/null 2>&1 && echo @@OK`

// eachServer выполняет f на всех серверах агента параллельно и собирает итоги.
func eachServer(p *Profile, f func(role string, c *conn) string) map[string]string {
	res := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, role := range agentServers(p) {
		wg.Add(1)
		go func(role string) {
			defer wg.Done()
			c := getConn(p, role)
			s := f(role, c)
			mu.Lock()
			res[role] = s
			mu.Unlock()
		}(role)
	}
	wg.Wait()
	return res
}

func handleAgentSet(m *mctx, r agentReq) (any, error) {
	r.Token, r.Chat = strings.TrimSpace(r.Token), strings.TrimSpace(r.Chat)
	if r.Token == "" { // пустое поле — оставить сохранённый токен
		out, _, _, err := m.ruCmd(`cat `+agentConf+` 2>/dev/null`, nil, 30*time.Second)
		if err != nil {
			return nil, err
		}
		r.Token = readKV(out)["TG_TOKEN"]
	}
	if !tokenRe.MatchString(r.Token) {
		return nil, errors.New("токен бота указан неверно — скопируйте его целиком из сообщения @BotFather (вида 123456789:AA…)")
	}
	if !chatRe.MatchString(r.Chat) {
		return nil, errors.New("укажите chat id — число (его можно определить кнопкой «Определить»)")
	}
	switch r.Backup {
	case "off", "daily", "weekly":
	default:
		r.Backup = "off"
	}
	if r.HourUTC < 0 || r.HourUTC > 23 {
		r.HourUTC = 1
	}
	if r.Expiry < 0 || r.Expiry > 30 {
		r.Expiry = 3
	}
	res := eachServer(m.p, func(role string, c *conn) string {
		if err := c.upload(agentPath, agentScript, "700"); err != nil {
			return err.Error()
		}
		if err := c.upload(agentConf, []byte(agentConfText(m.p, role, r)), "600"); err != nil {
			return err.Error()
		}
		out, se, _, err := c.lockedRun(agentUnitsCmd, nil, time.Minute)
		if err != nil {
			return err.Error()
		}
		if !strings.Contains(out, "@@OK") {
			return "не удалось включить таймер: " + tail(se+out, 2)
		}
		return ""
	})
	logLine(fmt.Sprintf("[Агент] «%s»: включён на %s", m.p.Name, strings.Join(agentServers(m.p), ", ")))
	return map[string]any{"ok": true, "results": res}, nil
}

func handleAgentOff(m *mctx, _ agentReq) (any, error) {
	res := eachServer(m.p, func(role string, c *conn) string {
		if _, _, _, err := c.lockedRun(agentUninst, nil, time.Minute); err != nil {
			return err.Error()
		}
		return ""
	})
	return map[string]any{"ok": true, "results": res}, nil
}

func agentRun(m *mctx, cmd string) map[string]string {
	return eachServer(m.p, func(role string, c *conn) string {
		out, _, _, err := c.lockedRun(`[ -f `+agentConf+` ] || { echo "@@FAIL агент на этом сервере не настроен — нажмите «Сохранить и включить»"; exit 0; }; bash `+agentPath+` `+cmd, nil, 3*time.Minute)
		if err != nil {
			return err.Error()
		}
		if strings.Contains(out, "@@OK") {
			return ""
		}
		if i := strings.Index(out, "@@FAIL"); i >= 0 {
			msg := strings.TrimSpace(out[i+6:])
			if msg == "" {
				msg = "Telegram не ответил (нет связи с api.telegram.org)"
			}
			return msg
		}
		return strings.TrimSpace(tail(out, 2))
	})
}

func handleAgentTest(m *mctx, _ agentReq) (any, error) {
	return map[string]any{"ok": true, "results": agentRun(m, "test")}, nil
}

func handleAgentBackup(m *mctx, _ agentReq) (any, error) {
	return map[string]any{"ok": true, "results": agentRun(m, "backup")}, nil
}

// handleAgentChat находит chat id: пользователь пишет боту, а мы читаем
// getUpdates с сервера RU (через туннель — из России Telegram может быть закрыт).
func handleAgentChat(m *mctx, r agentReq) (any, error) {
	tok := strings.TrimSpace(r.Token)
	if tok == "" {
		out, _, _, err := m.ruCmd(`cat `+agentConf+` 2>/dev/null`, nil, 30*time.Second)
		if err != nil {
			return nil, err
		}
		tok = readKV(out)["TG_TOKEN"]
	}
	if !tokenRe.MatchString(tok) {
		return nil, errors.New("сначала вставьте токен бота")
	}
	out, _, _, err := m.ruCmd(`T=`+shq(tok)+`
for i in awg1 ""; do
  if [ -n "$i" ]; then ip link show "$i" >/dev/null 2>&1 || continue; fi
  o=$(curl -s -m 30 ${i:+--interface $i} "https://api.telegram.org/bot$T/getUpdates") || continue
  case "$o" in *'"ok":true'*) printf '%s' "$o"; exit 0;; esac
  echo "@@ERR $o" | head -c 300
done`, nil, 2*time.Minute)
	if err != nil {
		return nil, err
	}
	if strings.Contains(out, "@@ERR") || !strings.Contains(out, `"ok":true`) {
		if strings.Contains(out, "Unauthorized") {
			return nil, errors.New("Telegram не принял токен — проверьте, что он скопирован целиком")
		}
		return nil, errors.New("не удалось связаться с Telegram с сервера RU")
	}
	var u struct {
		Result []struct {
			Message *struct {
				Chat struct {
					ID    int64  `json:"id"`
					Title string `json:"title"`
					First string `json:"first_name"`
					User  string `json:"username"`
				} `json:"chat"`
			} `json:"message"`
		} `json:"result"`
	}
	_ = json.Unmarshal([]byte(out), &u)
	for i := len(u.Result) - 1; i >= 0; i-- {
		if msg := u.Result[i].Message; msg != nil {
			who := strings.TrimSpace(msg.Chat.Title + " " + msg.Chat.First)
			if msg.Chat.User != "" {
				who += " (@" + msg.Chat.User + ")"
			}
			return map[string]any{"chat": strconv.FormatInt(msg.Chat.ID, 10), "who": strings.TrimSpace(who)}, nil
		}
	}
	return nil, errors.New("бот пока не получил сообщений — откройте его в Telegram, нажмите «Старт» (или напишите любое сообщение) и попробуйте ещё раз")
}

// ---------- лимиты трафика ----------

type limitRow struct {
	Name    string  `json:"name"`
	LimitGB float64 `json:"limitGb"`
	Used    int64   `json:"used"` // байт за текущий месяц
}

func handleLimitsGet(m *mctx, _ agentReq) (any, error) {
	out, _, _, err := m.ruCmd(`echo "@@L"; cat `+limitsFile+` 2>/dev/null; echo "@@U"; M=$(date +%Y-%m); awk -F'\t' -v m="$M" 'substr($1,1,7)==m{u[$2]+=$3+$4} END{for(n in u) print n"\t"u[n]}' /root/awg/awgc-stats/daily.tsv 2>/dev/null
echo "@@N"; awk '/^#_Name = /{print substr($0,10)}' /etc/amnezia/amneziawg/awg0.conf 2>/dev/null; echo "@@M $(date +%Y-%m)"`, nil, time.Minute)
	if err != nil {
		return nil, err
	}
	sec := func(a, b string) string {
		i := strings.Index(out, a)
		if i < 0 {
			return ""
		}
		s := out[i+len(a):]
		if j := strings.Index(s, b); j >= 0 {
			s = s[:j]
		}
		return s
	}
	lim := map[string]float64{}
	for _, l := range strings.Split(sec("@@L", "@@U"), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && !strings.HasPrefix(f[0], "#") {
			if v, err := strconv.ParseFloat(f[1], 64); err == nil {
				lim[f[0]] = v
			}
		}
	}
	used := map[string]int64{}
	for _, l := range strings.Split(sec("@@U", "@@N"), "\n") {
		f := strings.Split(strings.TrimSpace(l), "\t")
		if len(f) == 2 {
			v, _ := strconv.ParseFloat(f[1], 64)
			used[f[0]] = int64(v)
		}
	}
	rows := []limitRow{}
	for _, n := range strings.Split(sec("@@N", "@@M"), "\n") {
		n = strings.TrimSpace(n)
		if n == "" || n == "ru_host" || n == "mid_host" {
			continue
		}
		rows = append(rows, limitRow{Name: n, LimitGB: lim[n], Used: used[n]})
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].Name < rows[b].Name })
	return map[string]any{"rows": rows, "month": strings.TrimSpace(kvLines(out)["M"])}, nil
}

func handleLimitsSet(m *mctx, r agentReq) (any, error) {
	var b strings.Builder
	b.WriteString("# Месячные лимиты трафика, ГБ (пишет программа AmneziaWG Cascade)\n")
	names := make([]string, 0, len(r.LimitGB))
	for n := range r.LimitGB {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v := r.LimitGB[n]
		if !nameRe.MatchString(n) || v < 0 || v > 1e6 {
			return nil, fmt.Errorf("неверный лимит для %q", n)
		}
		if v > 0 {
			fmt.Fprintf(&b, "%s %s\n", n, strconv.FormatFloat(v, 'f', -1, 64))
		}
	}
	c := getConn(m.p, "RU")
	if err := c.upload(limitsFile, []byte(b.String()), "600"); err != nil {
		return nil, fmt.Errorf("сервер RU: %v", err)
	}
	return map[string]any{"ok": true}, nil
}

func aapi(f func(m *mctx, r agentReq) (any, error)) func(w http.ResponseWriter, r *http.Request) {
	return api(func(w http.ResponseWriter, r *http.Request) {
		var ar agentReq
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&ar); err != nil {
			writeJSON(w, map[string]string{"error": "неверный запрос"})
			return
		}
		p, err := getProfile(ar.ID)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		res, err := f(&mctx{p: p}, ar)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, res)
	})
}

func registerAgent(mux *http.ServeMux) {
	mux.HandleFunc("/api/m/agent-get", aapi(handleAgentGet))
	mux.HandleFunc("/api/m/agent-set", aapi(handleAgentSet))
	mux.HandleFunc("/api/m/agent-off", aapi(handleAgentOff))
	mux.HandleFunc("/api/m/agent-test", aapi(handleAgentTest))
	mux.HandleFunc("/api/m/agent-backup", aapi(handleAgentBackup))
	mux.HandleFunc("/api/m/agent-chat", aapi(handleAgentChat))
	mux.HandleFunc("/api/m/limits-get", aapi(handleLimitsGet))
	mux.HandleFunc("/api/m/limits-set", aapi(handleLimitsSet))
}
