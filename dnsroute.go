// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

// «Домены РФ»: маршрутизация российских сервисов по доменам. Сервер RU становится
// DNS-сервером для пользователей VPN; адреса российских доменов попадают в набор
// ru_dns и идут напрямую, даже если сервис стоит на зарубежном CDN.
// Вся логика на сервере — в assets/awgc-dns.sh.

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed assets/awgc-dns.sh
var dnsScript []byte

const dnsScriptPath = "/root/awg/awgc-dns.sh"

type dnsReq struct {
	ID     string `json:"id"`
	Action string `json:"action"` // enable | apply | disable
	TLDs   bool   `json:"tlds"`
	Custom string `json:"custom"`
}

var domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// parseDomains: свои домены пользователя, по одному на строку (или через пробел/запятую).
func parseDomains(s string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' || r == ';'
	}) {
		f = strings.TrimSpace(f)
		f = strings.TrimPrefix(strings.TrimPrefix(f, "https://"), "http://")
		if i := strings.IndexAny(f, "/:?"); i >= 0 {
			f = f[:i]
		}
		f = strings.TrimPrefix(strings.TrimPrefix(f, "*."), ".")
		f = strings.TrimSuffix(f, ".")
		if f == "" || strings.HasPrefix(f, "#") {
			continue
		}
		if !domainRe.MatchString(f) {
			return nil, fmt.Errorf("«%s» — не похоже на домен (пример: example.ru; русские домены — в виде xn--…)", f)
		}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	if len(out) > 500 {
		return nil, errors.New("слишком много своих доменов (больше 500)")
	}
	return out, nil
}

func (m *mctx) uploadDNSScript() error {
	c := getConn(m.p, "RU")
	if err := c.upload(dnsScriptPath, dnsScript, "700"); err != nil {
		return fmt.Errorf("сервер RU: %v", err)
	}
	return nil
}

func handleDNSGet(m *mctx, _ dnsReq) (any, error) {
	if err := m.uploadDNSScript(); err != nil {
		return nil, err
	}
	out, _, _, err := m.ruCmd("bash "+dnsScriptPath+" status; [ -f /root/awg/awg-routing.sh ] && echo '@@CASCADE 1'", nil, time.Minute)
	if err != nil {
		return nil, err
	}
	kv := kvLines(out)
	custom := strings.TrimSpace(between(out, "@@CUSTOM", "@@END"))
	n := func(k string) int { v, _ := strconv.Atoi(kv[k]); return v }
	return map[string]any{
		"cascade":  kv["CASCADE"] == "1",
		"enabled":  kv["ENABLED"] == "1",
		"active":   kv["ACTIVE"] == "1",
		"addr":     kv["ADDR"],
		"domains":  n("DOMAINS"),
		"ips":      n("IPS"),
		"listDate": kv["LISTDATE"],
		"tlds":     kv["TLDS"] == "1",
		"dns":      kv["DNS"],
		"custom":   custom,
	}, nil
}

func handleDNSSet(m *mctx, r dnsReq) (any, error) {
	switch r.Action {
	case "enable", "apply", "disable":
	default:
		return nil, errors.New("неизвестное действие")
	}
	doms, err := parseDomains(r.Custom)
	if err != nil {
		return nil, err
	}
	if err := m.uploadDNSScript(); err != nil {
		return nil, err
	}
	tlds := "rm -f /root/awg/awgc-dns-tlds"
	if r.TLDs {
		tlds = "touch /root/awg/awgc-dns-tlds"
	}
	cmd := fmt.Sprintf(`set -e
printf '%%s' %s > /root/awg/awgc-dns-custom.txt
%s
bash %s %s 2>&1`, shq(strings.Join(doms, "\n")+"\n"), tlds, dnsScriptPath, r.Action)
	out, se, code, err := m.ruCmd(cmd, nil, 15*time.Minute)
	if err != nil {
		return nil, err
	}
	if code != 0 || !strings.Contains(out, "@@OK") {
		return nil, fmt.Errorf("не получилось: %s", tail(se+out, 4))
	}
	res := map[string]any{"ok": true, "action": r.Action}
	var fails []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "@@CLIENTS "):
			var ok, bad int
			fmt.Sscanf(l, "@@CLIENTS %d %d", &ok, &bad)
			res["updated"] = ok
		case strings.HasPrefix(l, "@@FAIL "):
			fails = append(fails, strings.TrimPrefix(l, "@@FAIL "))
		case strings.HasPrefix(l, "@@DOMAINS "):
			res["domains"], _ = strconv.Atoi(strings.TrimPrefix(l, "@@DOMAINS "))
		}
	}
	res["failed"] = fails
	logLine(fmt.Sprintf("[RU] Домены РФ: %s (своих доменов %d, зоны .ru/.рф/.su: %v)", r.Action, len(doms), r.TLDs))
	return res, nil
}

func dapi(f func(m *mctx, r dnsReq) (any, error)) func(w http.ResponseWriter, r *http.Request) {
	return api(func(w http.ResponseWriter, r *http.Request) {
		var dr dnsReq
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<17)).Decode(&dr); err != nil {
			writeJSON(w, map[string]string{"error": "неверный запрос"})
			return
		}
		p, err := getProfile(dr.ID)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		res, err := f(&mctx{p: p}, dr)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, res)
	})
}

func registerDNS(mux *http.ServeMux) {
	mux.HandleFunc("/api/m/dns-get", dapi(handleDNSGet))
	mux.HandleFunc("/api/m/dns-set", dapi(handleDNSSet))
}
