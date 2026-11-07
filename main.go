// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// version задаётся при сборке: -ldflags "-X main.version=1.4.0"
var version = "1.12.0"

var (
	token   string
	hub     = NewHub()
	jobMu   sync.Mutex
	jobStop context.CancelFunc
	lastDir string
	logFile *os.File
	logMu   sync.Mutex
)

func main() {
	addr := fmt.Sprintf("127.0.0.1:%d", appPort)
	if v := os.Getenv("AWGC_ADDR"); v != "" { // для тестов
		addr = v
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Программа уже запущена. Та же версия — просто открываем её окно ещё раз;
		// другая (например, старая после обновления) — закрываем её и запускаемся сами.
		if rv := runningVersion(addr); (rv != "" && !versionLess(rv, version)) || !stopOldInstance(appPort) {
			openBrowser("http://" + addr + "/")
			return
		}
		for i := 0; i < 50; i++ {
			if ln, err = net.Listen("tcp", addr); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			openBrowser("http://" + addr + "/")
			return
		}
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	token = hex.EncodeToString(b)

	setupLogFile()
	logLine(fmt.Sprintf("======== Запуск программы %s v%s ========", appTitle, version))

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	// Версия без токена: новая копия программы узнаёт, какая версия уже запущена.
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"app": appID, "version": version, "edition": edition})
	})
	mux.HandleFunc("/api/start", api(handleStart))
	mux.HandleFunc("/api/cancel", api(handleCancel))
	mux.HandleFunc("/api/events", handleEvents)
	mux.HandleFunc("/api/open-folder", api(handleOpenFolder))
	mux.HandleFunc("/api/open-log", api(handleOpenLog))
	mux.HandleFunc("/api/ipcheck", api(handleIPCheck))
	registerManage(mux)
	mux.HandleFunc("/api/state", api(func(w http.ResponseWriter, r *http.Request) {
		hub.mu.Lock()
		running := hub.running
		hub.mu.Unlock()
		writeJSON(w, map[string]any{"running": running})
	}))

	srv := &http.Server{Handler: hostGuard(addr, actionLog(mux))}
	url := "http://" + addr + "/"
	if os.Getenv("AWGC_NO_BROWSER") == "" {
		go func() { time.Sleep(300 * time.Millisecond); openBrowser(url) }()
	}
	// Закрываемся, когда окно закрыто и ничего не выполняется.
	go func() {
		time.Sleep(90 * time.Second)
		for {
			time.Sleep(5 * time.Second)
			if hub.Idle(60 * time.Second) {
				os.Exit(0)
			}
		}
	}()
	_ = srv.Serve(ln)
}

// runningVersion — версия уже запущенной копии ("" — старая, до 1.5.1, или не ответила).
func runningVersion(addr string) string {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + "/api/version")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var v struct{ App, Version string }
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&v) != nil || v.App != appID {
		return ""
	}
	return v.Version
}

// versionLess: a < b для версий вида 1.5.1.
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			fmt.Sscan(pa[i], &x)
		}
		if i < len(pb) {
			fmt.Sscan(pb[i], &y)
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func setupLogFile() {
	path := logPath()
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	rotateLog(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	logFile = f
	hub.logSink = func(s string) {
		logMu.Lock()
		fmt.Fprintln(logFile, s)
		logMu.Unlock()
	}
}

// hostGuard отсекает запросы с чужим Host (защита от DNS-rebinding).
func hostGuard(addr string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != addr && !strings.HasPrefix(r.Host, "localhost:") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// api требует секретный токен из страницы — чужие сайты не смогут управлять программой.
func api(f http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("X-Token") != token {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		f(w, r)
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
	fmt.Fprint(w, strings.NewReplacer("__TOKEN__", token, "__VERSION__", "v"+strings.TrimPrefix(version, "v")).Replace(indexHTML))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	var in JobInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSON(w, map[string]string{"error": "неверные данные формы"})
		return
	}
	if err := in.validate(); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if !hub.SetRunning(true) {
		writeJSON(w, map[string]string{"error": "установка уже идёт"})
		return
	}
	hub.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	jobMu.Lock()
	jobStop = cancel
	jobMu.Unlock()
	if logFile != nil {
		logMu.Lock()
		fmt.Fprintf(logFile, "\n======== %s · %s v%s ========\n", time.Now().Format("2006-01-02 15:04:05"), appTitle, version)
		logMu.Unlock()
	}
	opts := "версия протокола " + in.Protocol
	if in.SkipUpdate {
		opts += ", без обновления системы"
	}
	if in.threeHop() {
		hub.Log("", fmt.Sprintf("Старт (программа v%s): три сервера — RU = %s, транзит = %s, выход = %s, %s", version, in.RU.Host, in.EU.Host, in.EX.Host, opts))
	} else {
		hub.Log("", fmt.Sprintf("Старт (программа v%s): RU = %s, EU = %s, %s", version, in.RU.Host, in.EU.Host, opts))
	}
	go func() {
		defer hub.SetRunning(false)
		defer cancel()
		j := &Job{in: in, hub: hub, ctx: ctx}
		j.Run()
		lastDir = outputDir(in.RU.Host)
	}()
	writeJSON(w, map[string]string{"ok": "1"})
}

func handleCancel(w http.ResponseWriter, r *http.Request) {
	jobMu.Lock()
	if jobStop != nil {
		jobStop()
	}
	jobMu.Unlock()
	writeJSON(w, map[string]string{"ok": "1"})
}

func handleOpenFolder(w http.ResponseWriter, r *http.Request) {
	if lastDir != "" {
		openFolder(lastDir)
	}
	writeJSON(w, map[string]string{"ok": "1"})
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("token") != token {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no stream", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	c, backlog := hub.Subscribe()
	defer hub.Unsubscribe(c)
	for _, b := range backlog {
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-c:
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}
