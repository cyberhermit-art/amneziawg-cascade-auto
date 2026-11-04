// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

// «Мимо VPN»: сети, которые устройство открывает напрямую, минуя туннель.
// Нужно для сайтов, которые не пускают адреса хостингов (например, Госуслуги):
// через каскад они уходят с IP сервера RU и получают отказ, а напрямую — с
// домашнего/мобильного IP пользователя — работают.
//
// Реализация на стороне клиента: AllowedIPs = базовый список (обычно 0.0.0.0/0)
// минус исключения. Сервер при этом ничего не меняет.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var presets = map[string]struct {
	Title string
	Nets  []string
}{
	"gosuslugi": {"Госуслуги (вход через ЕСИА, личный кабинет, сервисы)", []string{"213.59.252.0/22", "109.207.0.0/20"}},
}

const (
	exclFile    = "/root/awg/awgc-exclude.txt"     // исключения, по одному на строку
	exclBase    = "/root/awg/awgc-exclude.base"    // исходный AllowedIPs клиентов
	exclAllowed = "/root/awg/awgc-exclude.allowed" // готовый AllowedIPs для новых клиентов
)

// ---------- арифметика префиксов ----------

func pfxRange(p netip.Prefix) (*big.Int, *big.Int) {
	p = p.Masked()
	a := p.Addr().As4()
	lo := new(big.Int).SetBytes(a[:])
	size := new(big.Int).Lsh(big.NewInt(1), uint(32-p.Bits()))
	hi := new(big.Int).Sub(new(big.Int).Add(lo, size), big.NewInt(1))
	return lo, hi
}

func intToAddr(x *big.Int) netip.Addr {
	b := x.FillBytes(make([]byte, 4))
	return netip.AddrFrom4([4]byte{b[0], b[1], b[2], b[3]})
}

// rangeToPrefixes — минимальный набор префиксов, покрывающий [lo, hi].
func rangeToPrefixes(lo, hi *big.Int) []netip.Prefix {
	var out []netip.Prefix
	one := big.NewInt(1)
	cur := new(big.Int).Set(lo)
	for cur.Cmp(hi) <= 0 {
		bits := 32
		for bits > 0 {
			size := new(big.Int).Lsh(one, uint(32-bits+1))
			if new(big.Int).Mod(cur, size).Sign() != 0 {
				break
			}
			end := new(big.Int).Sub(new(big.Int).Add(cur, size), one)
			if end.Cmp(hi) > 0 {
				break
			}
			bits--
		}
		out = append(out, netip.PrefixFrom(intToAddr(cur), bits))
		cur.Add(cur, new(big.Int).Lsh(one, uint(32-bits)))
	}
	return out
}

// subtract: base минус excl (только IPv4; IPv6-элементы базы сохраняются как есть).
func subtract(base []netip.Prefix, excl []netip.Prefix) []netip.Prefix {
	type rng struct{ lo, hi *big.Int }
	var ranges []rng
	for _, b := range base {
		lo, hi := pfxRange(b)
		ranges = append(ranges, rng{lo, hi})
	}
	one := big.NewInt(1)
	for _, e := range excl {
		elo, ehi := pfxRange(e)
		var next []rng
		for _, r := range ranges {
			if ehi.Cmp(r.lo) < 0 || elo.Cmp(r.hi) > 0 {
				next = append(next, r)
				continue
			}
			if elo.Cmp(r.lo) > 0 {
				next = append(next, rng{r.lo, new(big.Int).Sub(elo, one)})
			}
			if ehi.Cmp(r.hi) < 0 {
				next = append(next, rng{new(big.Int).Add(ehi, one), r.hi})
			}
		}
		ranges = next
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].lo.Cmp(ranges[j].lo) < 0 })
	var out []netip.Prefix
	for _, r := range ranges {
		out = append(out, rangeToPrefixes(r.lo, r.hi)...)
	}
	return out
}

var cidrRe = regexp.MustCompile(`^[0-9.]+(/[0-9]{1,2})?$`)

// parseNets разбирает список IPv4-адресов/сетей (через запятую, пробел или с новой строки).
func parseNets(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	seen := map[netip.Prefix]bool{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' || r == ';' }) {
		f = strings.TrimSpace(f)
		if f == "" || strings.HasPrefix(f, "#") {
			continue
		}
		if !cidrRe.MatchString(f) {
			return nil, fmt.Errorf("«%s» — не IPv4-адрес или сеть (пример: 213.59.252.0/22)", f)
		}
		if !strings.Contains(f, "/") {
			f += "/32"
		}
		p, err := netip.ParsePrefix(f)
		if err != nil || !p.Addr().Is4() {
			return nil, fmt.Errorf("«%s» — не IPv4-сеть", f)
		}
		p = p.Masked()
		if p.Bits() < 8 {
			return nil, fmt.Errorf("сеть %s слишком большая", p)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}

func joinPfx(ps []netip.Prefix) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = p.String()
	}
	return strings.Join(s, ", ")
}

// ---------- API ----------

type exclReq struct {
	ID   string `json:"id"`
	Nets string `json:"nets"`
	Host string `json:"host"`
}

func handleExclGet(m *mctx, r exclReq) (any, error) {
	out, _, _, err := m.ruCmd(`echo "@@NETS"; cat `+exclFile+` 2>/dev/null; echo "@@BASE"; cat `+exclBase+` 2>/dev/null; echo "@@END"`, nil, time.Minute)
	if err != nil {
		return nil, err
	}
	nets := between(out, "@@NETS", "@@BASE")
	base := strings.TrimSpace(between(out, "@@BASE", "@@END"))
	pl := []map[string]any{}
	ids := make([]string, 0, len(presets))
	for id := range presets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := presets[id]
		pl = append(pl, map[string]any{"id": id, "title": p.Title, "nets": p.Nets})
	}
	return map[string]any{"nets": strings.TrimSpace(nets), "base": base, "presets": pl}, nil
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	if j := strings.Index(s, b); j >= 0 {
		s = s[:j]
	}
	return s
}

// handleExclSet пересчитывает AllowedIPs и применяет его ко всем пользователям.
func handleExclSet(m *mctx, r exclReq) (any, error) {
	excl, err := parseNets(r.Nets)
	if err != nil {
		return nil, err
	}
	if len(excl) > 40 {
		return nil, errors.New("слишком много исключений (больше 40) — QR-коды перестанут помещаться")
	}
	// Базовый AllowedIPs: сохранённый при первом применении, иначе — из файла первого клиента.
	out, _, _, err := m.ruCmd(`if [ -s `+exclBase+` ]; then cat `+exclBase+`; else
f=$(ls /root/awg/*.conf 2>/dev/null | grep -v '/awgc-' | head -1); [ -n "$f" ] && sed -n 's/^AllowedIPs[ \t]*=[ \t]*//p' "$f" | head -1; fi
f=$(ls /root/awg/*.conf 2>/dev/null | grep -v '/awgc-' | head -1)
[ -n "$f" ] && echo "@@OTHER $(grep -v '^AllowedIPs' "$f" | wc -c)"`, nil, time.Minute)
	if err != nil {
		return nil, err
	}
	other := 900 // размер файла подключения без строки AllowedIPs, байт (если не удалось прочитать)
	if i := strings.Index(out, "@@OTHER "); i >= 0 {
		fmt.Sscanf(out[i:], "@@OTHER %d", &other)
		out = out[:i]
	}
	baseStr := strings.TrimSpace(out)
	if baseStr == "" {
		baseStr = "0.0.0.0/0"
	}
	var base4 []netip.Prefix
	var keep []string // IPv6-части базы остаются как есть
	for _, f := range strings.Split(baseStr, ",") {
		f = strings.TrimSpace(f)
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("не удалось разобрать текущий AllowedIPs клиентов: %q", baseStr)
		}
		if p.Addr().Is4() {
			base4 = append(base4, p)
		} else {
			keep = append(keep, p.String())
		}
	}
	allowed := baseStr
	if len(excl) > 0 {
		allowed = joinPfx(subtract(base4, excl))
		if len(keep) > 0 {
			allowed += ", " + strings.Join(keep, ", ")
		}
	}
	// QR-код вмещает около 2900 байт. Файл подключения с длинным AllowedIPs в него не
	// помещается, а без QR установщик (на 3.1) не создаёт нового пользователя совсем.
	const qrLimit = 2700
	if size := other + len("AllowedIPs = ") + len(allowed) + 1; len(excl) > 0 && size > qrLimit {
		n := len(strings.Split(allowed, ","))
		return nil, fmt.Errorf("слишком много сетей мимо VPN: в файле подключения получится %d маршрутов (около %d байт), а QR-код вмещает примерно %d. С таким списком у пользователей не будет QR-кода, а новых пользователей сервер не сможет добавить. Уберите часть сетей — каждая сеть добавляет до 20–30 маршрутов", n, size, qrLimit)
	}
	var lines []string
	for _, p := range excl {
		lines = append(lines, p.String())
	}
	script := fmt.Sprintf(`set -u
A=/root/awg; M=%[1]s
[ -s %[2]s ] || printf '%%s\n' %[5]s > %[2]s
printf '%%s' %[6]s > %[3]s.new && mv -f %[3]s.new %[3]s
if [ %[7]d -eq 0 ]; then rm -f %[4]s; else printf '%%s' %[8]s > %[4]s; fi
ok=0; bad=0
for n in $(grep '^#_Name = ' /etc/amnezia/amneziawg/awg0.conf | sed 's/^#_Name = //'); do
  if bash $M --yes --no-color modify "$n" AllowedIPs %[8]s >/tmp/awgc-mod.log 2>&1; then ok=$((ok+1));
    [ -f "$A/$n.png" ] || echo "@@NOQR $n"
  else bad=$((bad+1)); echo "@@FAIL $n $(tail -1 /tmp/awgc-mod.log)"; fi
done
echo "@@DONE $ok $bad"`, manageSh, exclBase, exclFile, exclAllowed, shq(baseStr), shq(strings.Join(lines, "\n")+"\n"), len(excl), shq(allowed))
	out, se, code, err := m.ruCmd(script, nil, 10*time.Minute)
	if err != nil {
		return nil, err
	}
	if code != 0 || !strings.Contains(out, "@@DONE") {
		return nil, fmt.Errorf("не удалось применить: %s", tail(se+out, 4))
	}
	var fails, noqr []string
	ok, bad := 0, 0
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "@@FAIL "):
			fails = append(fails, strings.TrimPrefix(l, "@@FAIL "))
		case strings.HasPrefix(l, "@@NOQR "):
			noqr = append(noqr, strings.TrimPrefix(l, "@@NOQR "))
		case strings.HasPrefix(l, "@@DONE "):
			fmt.Sscanf(l, "@@DONE %d %d", &ok, &bad)
		}
	}
	return map[string]any{"ok": true, "updated": ok, "failed": fails, "noQR": noqr, "entries": len(strings.Split(allowed, ","))}, nil
}

var hostRe = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)

// handleCheckSite проверяет сайт с сервера RU: адреса, попадание в список RU-сетей, доступность.
func handleCheckSite(m *mctx, r exclReq) (any, error) {
	h := strings.TrimSpace(strings.ToLower(r.Host))
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	if i := strings.IndexAny(h, "/:?"); i >= 0 {
		h = h[:i]
	}
	if !hostRe.MatchString(h) || !strings.Contains(h, ".") {
		return nil, errors.New("введите адрес сайта, например esia.gosuslugi.ru")
	}
	out, _, _, err := m.ruCmd(fmt.Sprintf(`H=%s
for ip in $(getent ahostsv4 "$H" | awk '{print $1}' | sort -u); do
  if ipset test ru "$ip" >/dev/null 2>&1; then echo "@@IP $ip ru"
  elif ipset test ru_dns "$ip" >/dev/null 2>&1; then echo "@@IP $ip rudns"
  else echo "@@IP $ip abroad"; fi
done
L=/etc/awgc-dns/domains.txt
if [ -s $L ] && systemctl is-active --quiet awgc-dns; then
  d=$H; while :; do grep -qxF "$d" $L && { echo "@@INLIST $d"; break; }; case $d in *.*) d=${d#*.};; *) break;; esac; done
fi
echo "@@CURL $(curl -4 -m 12 -A 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36' -o /dev/null -s -w '%%{http_code} %%{time_connect}' "https://$H/" 2>/dev/null || true)"`, shq(h)), nil, time.Minute)
	if err != nil {
		return nil, err
	}
	type ipInfo struct {
		IP   string `json:"ip"`
		Path string `json:"path"`
	}
	var ips []ipInfo
	code := "000"
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) >= 3 && f[0] == "@@IP" {
			ips = append(ips, ipInfo{f[1], f[2]})
		} else if strings.HasPrefix(l, "@@CURL") {
			if f := strings.Fields(strings.TrimPrefix(l, "@@CURL")); len(f) > 0 {
				code = f[0]
			}
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("сервер RU не смог найти адрес %s", h)
	}
	// Открывается — только ответы 2xx/3xx. 4xx/5xx (403, 429, 498 у антибота
	// Wildberries и т. п.) — сайт отвечает, но отказывает адресу сервера RU.
	n, _ := strconv.Atoi(code)
	reach := n >= 200 && n < 400
	refused := n >= 400
	inList := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "@@INLIST ") {
			inList = strings.TrimSpace(strings.TrimPrefix(l, "@@INLIST "))
		}
	}
	return map[string]any{"host": h, "ips": ips, "code": code, "reachable": reach, "refused": refused, "inList": inList}, nil
}

// allowedOpt — фрагмент команды add: новые клиенты сразу получают AllowedIPs с исключениями.
// Используется как: allowedPre + "\n" + "bash manage ... ${AIP:+\"$AIP\"} add NAME".
const allowedPre = `AIP=; [ -s ` + exclAllowed + ` ] && AIP="--allowed-ips=$(cat ` + exclAllowed + `)"`

func xapi(f func(m *mctx, r exclReq) (any, error)) func(w http.ResponseWriter, r *http.Request) {
	return api(func(w http.ResponseWriter, r *http.Request) {
		var xr exclReq
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&xr); err != nil {
			writeJSON(w, map[string]string{"error": "неверный запрос"})
			return
		}
		p, err := getProfile(xr.ID)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		res, err := f(&mctx{p: p}, xr)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, res)
	})
}

func registerExclude(mux *http.ServeMux) {
	mux.HandleFunc("/api/m/excl-get", xapi(handleExclGet))
	mux.HandleFunc("/api/m/excl-set", xapi(handleExclSet))
	mux.HandleFunc("/api/m/check-site", xapi(handleCheckSite))
}
