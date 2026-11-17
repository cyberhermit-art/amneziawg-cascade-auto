// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// version задаётся при сборке: -ldflags "-X main.version=1.4.0"
var version = "1.18.0"

var (
	token string
	// launchKey — одноразовый ключ запуска в адресе страницы (?k=…); хранится в
	// папке пользователя, поэтому другая учётная запись Windows его не узнает.
	// session — значение cookie, которое страница получает в обмен на ключ.
	launchKey string
	session   string
	hub       = NewHub()
	jobMu     sync.Mutex
	jobStop   context.CancelFunc
	lastDir   string
	logFile   *os.File
	logMu     sync.Mutex
)

func main() {
	addr := fmt.Sprintf("127.0.0.1:%d", appPort)
	fixed := os.Getenv("AWGC_ADDR") != "" // для тестов
	if fixed {
		addr = os.Getenv("AWGC_ADDR")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil && portAnswers(addr) {
		// Порт занят процессом другой учётной записи — это не наша программа.
		if !listenerIsMine(appPort) {
			showError(appTitle, fmt.Sprintf("Порт %d на этом компьютере занят программой другого пользователя Windows. Откройте AmneziaWG Cascade после её закрытия или из своей учётной записи, где программа ещё не запущена.", appPort))
			return
		}
		// Программа уже запущена. Та же версия — просто открываем её окно ещё раз;
		// другая (например, старая после обновления) — закрываем её и запускаемся сами.
		if rv := runningVersion(addr); (rv != "" && !versionLess(rv, version)) || !stopOldInstance(appPort) {
			openBrowser(launchURL(addr, readLaunchKey()))
			return
		}
		for i := 0; i < 50; i++ {
			if ln, err = net.Listen("tcp", addr); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			openBrowser(launchURL(addr, readLaunchKey()))
			return
		}
	} else if err != nil && !fixed {
		// Порт никто не слушает, но Windows его не даёт: он попал в диапазон,
		// зарезервированный системой (Hyper-V, WSL, Docker, песочница Windows).
		// Тогда работаем на любом свободном порту; адрес — в файле, чтобы
		// следующий запуск нашёл уже открытую программу.
		if alt := readSavedAddr(); alt != "" && alt != addr && portAnswers(alt) {
			if rv := runningVersion(alt); rv != "" && listenerIsMine(portOf(alt)) {
				if !versionLess(rv, version) || !stopOldInstance(portOf(alt)) {
					openBrowser(launchURL(alt, readLaunchKey()))
					return
				}
			}
		}
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err == nil {
			addr = ln.Addr().String()
		}
	}
	if err != nil {
		showError(appTitle, "Не удалось открыть локальный порт для окна программы: "+err.Error())
		return
	}
	saveAddr(addr)
	token, launchKey, session = randHex(16), randHex(16), randHex(16)
	writeLaunchKey(launchKey)

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
	// «Доверять новому ключу»: сервер переустановлен, его SSH-ключ сменился.
	mux.HandleFunc("/api/hostkey-forget", api(func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Addr string }
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&q) != nil || q.Addr == "" {
			writeJSON(w, map[string]string{"error": "неверный запрос"})
			return
		}
		if err := setHostKey(q.Addr, ""); err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		logLine("[SSH] новый ключ сервера " + q.Addr + " будет принят при следующем подключении")
		writeJSON(w, map[string]any{"ok": true})
	}))
	mux.HandleFunc("/api/state", api(func(w http.ResponseWriter, r *http.Request) {
		hub.mu.Lock()
		running := hub.running
		hub.mu.Unlock()
		writeJSON(w, map[string]any{"running": running})
	}))

	srv := &http.Server{Handler: hostGuard(addr, actionLog(mux))}
	url := launchURL(addr, launchKey)
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

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func launchKeyPath() string {
	return filepath.Join(appDataDir(), fmt.Sprintf("launch-%d.key", appPort))
}

func writeLaunchKey(k string) {
	if os.MkdirAll(appDataDir(), 0o700) == nil {
		_ = os.WriteFile(launchKeyPath(), []byte(k), 0o600)
	}
}

func readLaunchKey() string {
	b, _ := os.ReadFile(launchKeyPath())
	return strings.TrimSpace(string(b))
}

// portAnswers — на адресе кто-то принимает соединения.
func portAnswers(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 700*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func portOf(addr string) int {
	_, p, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(p)
	return n
}

// Адрес, на котором работает программа (обычно 127.0.0.1:47821, но может быть
// запасной, если Windows зарезервировала основной порт).
func addrPath() string { return filepath.Join(appDataDir(), fmt.Sprintf("addr-%d.txt", appPort)) }

func saveAddr(a string) {
	if os.MkdirAll(appDataDir(), 0o700) == nil {
		_ = os.WriteFile(addrPath(), []byte(a), 0o600)
	}
}

func readSavedAddr() string {
	b, _ := os.ReadFile(addrPath())
	a := strings.TrimSpace(string(b))
	if h, _, err := net.SplitHostPort(a); err != nil || h != "127.0.0.1" {
		return ""
	}
	return a
}

func launchURL(addr, k string) string {
	if k == "" {
		return "http://" + addr + "/"
	}
	return "http://" + addr + "/?k=" + k
}

func cookieName() string { return fmt.Sprintf("awgc%d", appPort) } // у Lite свой порт и своя cookie

func eq(a, b string) bool { return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// hasSession — запрос пришёл из страницы, открытой с ключом запуска.
func hasSession(r *http.Request) bool {
	c, err := r.Cookie(cookieName())
	return err == nil && eq(c.Value, session)
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
		if r.Method != http.MethodPost || !eq(r.Header.Get("X-Token"), token) || !hasSession(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// пока запрос выполняется (смена порта, исправления…), программа не закрывается
		hub.inflight.Add(1)
		defer hub.inflight.Add(-1)
		f(w, r)
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	// Ключ запуска → cookie сеанса и адрес без ключа.
	if k := r.URL.Query().Get("k"); k != "" {
		if eq(k, launchKey) {
			http.SetCookie(w, &http.Cookie{Name: cookieName(), Value: session, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	if !hasSession(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title><body style="font:16px system-ui;padding:40px;max-width:640px">
<h2>Откройте программу заново</h2><p>Эта страница открывается только из самой программы. Запустите %s ещё раз — окно откроется само.</p>
<p style="color:#777">Open the app again: this page only opens from the app itself.</p>`, appTitle, appTitle)
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
	var addExitP *Profile
	if in.Mode == "add-exit" {
		if edition != "full" {
			writeJSON(w, map[string]string{"error": "недоступно в этой версии"})
			return
		}
		p, err := in.validateAddExit()
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		addExitP = p
	} else if err := in.validate(); err != nil {
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
	if addExitP != nil {
		hub.Log("", fmt.Sprintf("Старт (программа v%s): резервный выход %s для каскада «%s»", version, in.RX.Host, addExitP.Name))
	} else if in.threeHop() {
		hub.Log("", fmt.Sprintf("Старт (программа v%s): три сервера — RU = %s, транзит = %s, выход = %s, %s", version, in.RU.Host, in.EU.Host, in.EX.Host, opts))
	} else {
		hub.Log("", fmt.Sprintf("Старт (программа v%s): RU = %s, EU = %s, %s", version, in.RU.Host, in.EU.Host, opts))
	}
	go func() {
		defer hub.SetRunning(false)
		defer cancel()
		j := &Job{in: in, hub: hub, ctx: ctx}
		j.Run()
		if addExitP == nil {
			lastDir = outputDir(in.RU.Host)
		}
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
	if !eq(r.URL.Query().Get("token"), token) || !hasSession(r) {
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
