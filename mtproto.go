// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

// MTProto-прокси для Telegram на сервере RU («Эксперименты», полная версия).
// На сервере работает mtg (github.com/9seconds/mtg) с FakeTLS; его соединения
// с Telegram уходят через туннель каскада. Вся логика — assets/awgc-mtproto.sh.
// Если сервер RU не может скачать mtg с GitHub, программа скачивает его сама и
// загружает по SSH.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed assets/awgc-mtproto.sh
var mtprotoScript []byte

const mtprotoPath = "/root/awg/awgc-mtproto.sh"

type mtpReq struct {
	ID     string `json:"id"`
	Enable bool   `json:"enable"`
	Port   int    `json:"port"`
	Domain string `json:"domain"`
}

var mtpDomainRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$`)

func handleMTPGet(m *mctx, _ mtpReq) (any, error) {
	c := getConn(m.p, "RU")
	if err := c.upload(mtprotoPath, mtprotoScript, "700"); err != nil {
		return nil, fmt.Errorf("сервер RU: %v", err)
	}
	out, _, _, err := m.ruCmd(`bash `+mtprotoPath+` status`, nil, time.Minute)
	if err != nil {
		return nil, err
	}
	kv := kvLines(out)
	port, _ := strconv.Atoi(kv["PORT"])
	conns, _ := strconv.Atoi(kv["CONNS"])
	return map[string]any{
		"enabled": kv["PORT"] != "", "active": kv["ACTIVE"] == "active",
		"port": port, "domain": kv["DOMAIN"], "link": kv["LINK"], "qr": kv["QR"], "conns": conns,
	}, nil
}

// mtgBinary скачивает mtg для linux/arch на этом компьютере (запасной путь).
func mtgBinary(arch string) ([]byte, error) {
	b, err := httpGet("https://api.github.com/repos/9seconds/mtg/releases/latest")
	if err != nil {
		return nil, err
	}
	var rel struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(b, &rel); err != nil {
		return nil, err
	}
	url := ""
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, "linux-"+arch+".tar.gz") {
			url = a.URL
		}
	}
	if url == "" {
		return nil, errors.New("в последнем выпуске mtg нет сборки linux-" + arch)
	}
	c := &http.Client{Timeout: 5 * time.Minute}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, errors.New("в архиве mtg нет программы")
		}
		if h.Typeflag == tar.TypeReg && (h.Name == "mtg" || strings.HasSuffix(h.Name, "/mtg")) {
			var buf bytes.Buffer
			if _, err := io.Copy(&buf, io.LimitReader(tr, 64<<20)); err != nil {
				return nil, err
			}
			return buf.Bytes(), nil
		}
	}
}

func handleMTPSet(m *mctx, r mtpReq) (any, error) {
	c := getConn(m.p, "RU")
	if err := c.upload(mtprotoPath, mtprotoScript, "700"); err != nil {
		return nil, fmt.Errorf("сервер RU: %v", err)
	}
	if !r.Enable {
		out, se, _, err := m.ruCmd(`bash `+mtprotoPath+` off 2>&1`, nil, time.Minute)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(out, "@@OK") {
			return nil, fmt.Errorf("сервер RU: %s", tail(se+out, 3))
		}
		logLine(fmt.Sprintf("[MTProto] «%s»: выключен", m.p.Name))
		return map[string]any{"ok": true}, nil
	}
	if r.Port == 0 {
		r.Port = 443
	}
	if r.Port < 1 || r.Port > 65535 {
		return nil, errors.New("неверный порт")
	}
	r.Domain = strings.ToLower(strings.TrimSpace(r.Domain))
	if r.Domain == "" {
		r.Domain = "ya.ru"
	}
	if !mtpDomainRe.MatchString(r.Domain) {
		return nil, errors.New("домен для маскировки указан неверно (пример: ya.ru)")
	}
	cmd := fmt.Sprintf(`bash %s on %d %s %s 2>&1`, mtprotoPath, r.Port, shq(r.Domain), shq(m.p.RU.Host))
	out, se, code, err := m.ruCmd(cmd, nil, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	if code == 3 && strings.Contains(out, "@@NEEDBIN") {
		arch := strings.TrimSpace(kvLines(out)["NEEDBIN"])
		bin, err := mtgBinary(arch)
		if err != nil {
			return nil, fmt.Errorf("сервер RU не смог скачать mtg с GitHub, и этот компьютер тоже: %v", err)
		}
		if err := c.upload("/usr/local/bin/awgc-mtg", bin, "755"); err != nil {
			return nil, fmt.Errorf("сервер RU: %v", err)
		}
		out, se, code, err = m.ruCmd(cmd, nil, 5*time.Minute)
		if err != nil {
			return nil, err
		}
	}
	if code != 0 || !strings.Contains(out, "@@OK") {
		return nil, fmt.Errorf("сервер RU: %s", tail(se+out, 3))
	}
	logLine(fmt.Sprintf("[MTProto] «%s»: включён, порт %d, маскировка под %s", m.p.Name, r.Port, r.Domain))
	return handleMTPGet(m, r)
}

func mtpapi(f func(m *mctx, r mtpReq) (any, error)) func(w http.ResponseWriter, r *http.Request) {
	return api(func(w http.ResponseWriter, r *http.Request) {
		var q mtpReq
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

func registerMTProto(mux *http.ServeMux) {
	mux.HandleFunc("/api/m/mtproto-get", mtpapi(handleMTPGet))
	mux.HandleFunc("/api/m/mtproto-set", mtpapi(handleMTPSet))
}
