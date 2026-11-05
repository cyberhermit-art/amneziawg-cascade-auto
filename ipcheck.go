// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

// Проверка адресов серверов до установки: считается ли адрес российским в
// разных базах. Для входа RU важно, чтобы его везде считали российским —
// иначе часть сервисов увидит «заграницу». Для выхода EU — наоборот.
//
// Базы (те же, что использует маршрутизация каскада):
//   - реестр RIPE / RIR — список ipdeny;
//   - MaxMind GeoLite2 — список antonme/GeoIP (его используют многие сервисы);
//   - DB-IP Lite — ru.txt проекта v2fly/geoip.
// Плюс ipinfo.io: сеть (ASN), владелец, тип и страна — если доступен.
// Списки кешируются в папке программы на 7 дней.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type ruList struct {
	ID, Title string
	URLs      []string
}

var ruLists = []ruList{
	{"rir", "Реестр RIPE (ipdeny)", []string{defaultRUZoneURL, "https://raw.githubusercontent.com/bivlked/amneziawg-installer/v5.37.0/cascade/ru.zone"}},
	{"maxmind", "MaxMind GeoLite2", []string{extraRUZoneURL}},
	{"dbip", "DB-IP", []string{"https://raw.githubusercontent.com/v2fly/geoip/release/text/ru.txt"}},
}

var (
	listMu    sync.Mutex
	listCache = map[string][]netip.Prefix{}
)

// loadRUList — сети списка: из памяти, из кеша на диске (до 7 дней) или скачав заново.
func loadRUList(l ruList) ([]netip.Prefix, error) {
	listMu.Lock()
	defer listMu.Unlock()
	if p, ok := listCache[l.ID]; ok {
		return p, nil
	}
	dir := filepath.Join(appDataDir(), "cache")
	_ = os.MkdirAll(dir, 0o700)
	file := filepath.Join(dir, "ru-"+l.ID+".txt")
	var data []byte
	if st, err := os.Stat(file); err == nil && time.Since(st.ModTime()) < 7*24*time.Hour {
		data, _ = os.ReadFile(file)
	}
	if len(data) == 0 {
		var lastErr error
		for _, u := range l.URLs {
			if b, err := httpGet(u); err == nil && bytes.Count(b, []byte("\n")) > 1000 {
				data = b
				_ = os.WriteFile(file, b, 0o600)
				break
			} else if err != nil {
				lastErr = err
			}
		}
		if len(data) == 0 {
			if b, err := os.ReadFile(file); err == nil { // устаревший кеш лучше, чем ничего
				data = b
			} else {
				if lastErr == nil {
					lastErr = errors.New("список пуст")
				}
				return nil, lastErr
			}
		}
	}
	var out []netip.Prefix
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") || strings.Contains(s, ":") {
			continue
		}
		if !strings.Contains(s, "/") {
			s += "/32"
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
		}
	}
	listCache[l.ID] = out
	return out, nil
}

type ipInfo struct {
	IP       string `json:"ip"`
	Country  string `json:"country"`
	City     string `json:"city"`
	Org      string `json:"org"`
	Hostname string `json:"hostname"`
}

func fetchIPInfo(ip string) (*ipInfo, error) {
	c := &http.Client{Timeout: 15 * time.Second}
	resp, err := c.Get("https://ipinfo.io/" + ip + "/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var v ipInfo
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, err
	}
	return &v, nil
}

type listResult struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	RU    *bool  `json:"ru"` // nil — база недоступна
	Err   string `json:"error,omitempty"`
}

type addrReport struct {
	Role     string       `json:"role"`
	Host     string       `json:"host"`
	IP       string       `json:"ip"`
	Lists    []listResult `json:"lists"`
	Country  string       `json:"country"`
	City     string       `json:"city"`
	Org      string       `json:"org"`
	ASN      string       `json:"asn"`
	Warnings []string     `json:"warnings"`
	OK       bool         `json:"ok"`
	Err      string       `json:"error,omitempty"`
}

func resolveV4(host string) (netip.Addr, error) {
	host = strings.TrimSpace(host)
	if a, err := netip.ParseAddr(host); err == nil {
		if !a.Is4() {
			return a, errors.New("нужен IPv4-адрес")
		}
		return a, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("не удалось найти адрес %s", host)
	}
	for _, ip := range ips {
		if v := ip.To4(); v != nil {
			return netip.AddrFrom4([4]byte{v[0], v[1], v[2], v[3]}), nil
		}
	}
	return netip.Addr{}, fmt.Errorf("у %s нет IPv4-адреса", host)
}

func checkAddr(role, host string) *addrReport {
	r := &addrReport{Role: role, Host: host, Warnings: []string{}}
	a, err := resolveV4(host)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	r.IP = a.String()
	if a.IsPrivate() || a.IsLoopback() || a.IsUnspecified() || a.IsLinkLocalUnicast() || a.IsMulticast() {
		r.Err = "это не публичный адрес — нужен внешний IPv4 сервера"
		return r
	}
	var wg sync.WaitGroup
	r.Lists = make([]listResult, len(ruLists))
	for i, l := range ruLists {
		wg.Add(1)
		go func(i int, l ruList) {
			defer wg.Done()
			res := listResult{ID: l.ID, Title: l.Title}
			nets, err := loadRUList(l)
			if err != nil {
				res.Err = "база недоступна"
			} else {
				in := false
				for _, p := range nets {
					if p.Contains(a) {
						in = true
						break
					}
				}
				res.RU = &in
			}
			r.Lists[i] = res
		}(i, l)
	}
	var info *ipInfo
	wg.Add(1)
	go func() { defer wg.Done(); info, _ = fetchIPInfo(r.IP) }()
	wg.Wait()
	if info != nil {
		r.Country, r.City, r.Org = info.Country, info.City, info.Org
		if f := strings.Fields(info.Org); len(f) > 0 && strings.HasPrefix(f[0], "AS") {
			r.ASN = f[0]
		}
	}

	known := 0
	for _, l := range r.Lists {
		if l.RU != nil {
			known++
		}
	}
	notRU := []string{}
	isRU := []string{}
	for _, l := range r.Lists {
		if l.RU != nil && !*l.RU {
			notRU = append(notRU, l.Title)
		}
		if l.RU != nil && *l.RU {
			isRU = append(isRU, l.Title)
		}
	}
	if role == "RU" {
		if len(notRU) > 0 {
			r.Warnings = append(r.Warnings, "Адрес не считается российским в базах: "+strings.Join(notRU, ", ")+". Сервисы, которые пользуются этими базами, увидят «заграницу» — для входа RU это плохо. Лучше взять сервер, у которого адрес везде российский.")
		}
		if r.Country != "" && r.Country != "RU" {
			r.Warnings = append(r.Warnings, "ipinfo.io считает адрес расположенным в стране "+r.Country+", а не в России.")
		}
	} else {
		if len(isRU) > 0 {
			r.Warnings = append(r.Warnings, "Адрес считается российским в базах: "+strings.Join(isRU, ", ")+". Для выхода EU нужен зарубежный адрес — иначе сайты увидят Россию.")
		}
		if r.Country == "RU" {
			r.Warnings = append(r.Warnings, "ipinfo.io считает адрес российским — для выхода EU нужен зарубежный.")
		}
	}
	if strings.Contains(strings.ToLower(r.Org), "aeza") {
		r.Warnings = append(r.Warnings, "Хостер Aeza: его подсети часто используют под VPN, и некоторые российские сервисы (например, Wildberries) не пускают такие адреса.")
	}
	if known == 0 && info == nil {
		r.Warnings = append(r.Warnings, "Не удалось проверить: базы недоступны с этого компьютера.")
	}
	r.OK = len(r.Warnings) == 0
	return r
}

type ipCheckReq struct {
	RU string `json:"ru"`
	EU string `json:"eu"`
}

func handleIPCheck(w http.ResponseWriter, r *http.Request) {
	var req ipCheckReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "неверный запрос"})
		return
	}
	var out []*addrReport
	var ru, eu *addrReport
	var wg sync.WaitGroup
	if strings.TrimSpace(req.RU) != "" {
		wg.Add(1)
		go func() { defer wg.Done(); ru = checkAddr("RU", req.RU) }()
	}
	if strings.TrimSpace(req.EU) != "" {
		wg.Add(1)
		go func() { defer wg.Done(); eu = checkAddr("EU", req.EU) }()
	}
	wg.Wait()
	if ru != nil && eu != nil && ru.ASN != "" && ru.ASN == eu.ASN {
		w := "Серверы RU и EU в одной сети (" + ru.ASN + "): если сервисы не доверяют этому хостеру, не поможет ни один из адресов. Лучше разные хостеры."
		ru.Warnings = append(ru.Warnings, w)
		eu.Warnings = append(eu.Warnings, w)
		ru.OK, eu.OK = false, false
	}
	for _, x := range []*addrReport{ru, eu} {
		if x != nil {
			out = append(out, x)
		}
	}
	warn := false
	for _, x := range out {
		if !x.OK || x.Err != "" {
			warn = true
		}
	}
	writeJSON(w, map[string]any{"reports": out, "warn": warn})
}
