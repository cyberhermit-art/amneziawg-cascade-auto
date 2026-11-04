// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

// Журнал действий: всё, что пользователь делает в программе (кроме простого
// просмотра), пишется в log.txt рядом с журналом установки — с датой, профилем,
// параметрами и результатом. Пароли, ключи и содержимое файлов не пишутся.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Какие запросы — действия и как они называются в журнале. Просмотр (состояние,
// список пользователей, статистика, опрос задач) сюда не входит — иначе журнал
// за день вырос бы до мегабайтов; ход задач пишется отдельно (logTaskChunk).
var actionNames = map[string]string{
	"/api/profile-save":     "Сохранение профиля",
	"/api/profile-delete":   "Удаление профиля",
	"/api/m/client":         "Просмотр ключа и QR-кода",
	"/api/m/add":            "Добавление пользователя",
	"/api/m/remove":         "Удаление пользователя",
	"/api/m/rename":         "Переименование пользователя",
	"/api/m/reissue":        "Перевыпуск ключа",
	"/api/m/restart-vpn":    "Перезапуск VPN",
	"/api/m/reboot":         "Перезагрузка сервера RU",
	"/api/m/server-reboot":  "Перезагрузка сервера",
	"/api/m/backup":         "Резервная копия",
	"/api/m/restore":        "Восстановление из копии",
	"/api/m/routing-update": "Обновление маршрутизации",
	"/api/m/task-start":     "Запуск задачи",
	"/api/m/excl-set":       "«Мимо VPN»: применение списка",
	"/api/m/check-site":     "Проверка сайта с сервера RU",
	"/api/m/ip-change":      "Смена IP-адреса сервера",
	"/api/m/dns-set":        "«Домены РФ»",
	"/api/cancel":           "Остановка установки",
}

var taskNames = map[string]string{"update": "обновление пакетов", "uninstall": "удаление VPN"}

// actReq — безопасные поля запроса, которые можно писать в журнал.
type actReq struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	NewName string `json:"newName"`
	Expires string `json:"expires"`
	Backup  string `json:"backup"`
	Target  string `json:"target"`
	Task    string `json:"task"`
	Reboot  bool   `json:"reboot"`
	NewHost string `json:"newHost"`
	Force   bool   `json:"force"`
	Host    string `json:"host"`
	Nets    string `json:"nets"`
	Action  string `json:"action"`
	TLDs    bool   `json:"tlds"`
	Custom  string `json:"custom"`
	RU      struct {
		Host string `json:"host"`
	} `json:"ru"`
	EU struct {
		Host string `json:"host"`
	} `json:"eu"`
}

func (a *actReq) describe(path string) string {
	var d []string
	add := func(k, v string) {
		if v != "" {
			d = append(d, k+" "+v)
		}
	}
	switch path {
	case "/api/profile-save":
		add("название", "«"+a.Name+"»")
		add("RU", a.RU.Host)
		add("EU", a.EU.Host)
	case "/api/m/rename":
		add("", a.Name+" → "+a.NewName)
	case "/api/m/add":
		add("", a.Name)
		add("срок", a.Expires)
	case "/api/m/task-start":
		t := taskNames[a.Task]
		if t == "" {
			t = a.Task
		}
		add("", t+" на сервере "+strings.ToUpper(a.Target))
		if a.Task == "update" && a.Reboot {
			d = append(d, "с перезагрузкой при необходимости")
		}
	case "/api/m/restore":
		add("копия", a.Backup)
		add("что", a.Target)
	case "/api/m/excl-set":
		n := len(strings.Fields(strings.NewReplacer(",", " ", ";", " ").Replace(a.Nets)))
		d = append(d, fmt.Sprintf("сетей мимо VPN: %d", n))
	case "/api/m/ip-change":
		add("сервер", strings.ToUpper(a.Target))
		add("новый адрес", a.NewHost)
		if a.Force {
			d = append(d, "принудительно")
		}
	case "/api/m/dns-set":
		act := map[string]string{"enable": "включение", "apply": "сохранение настроек", "disable": "выключение"}[a.Action]
		add("", act)
		n := len(strings.Fields(strings.NewReplacer(",", " ", ";", " ").Replace(a.Custom)))
		d = append(d, fmt.Sprintf("своих доменов: %d", n))
		if a.TLDs {
			d = append(d, "зоны .ru/.рф/.su целиком")
		}
	case "/api/m/server-reboot":
		add("", strings.ToUpper(a.Target))
	default:
		add("", a.Name)
		add("", a.Host)
	}
	return strings.TrimSpace(strings.Join(d, ", "))
}

// capWriter запоминает начало ответа, чтобы понять, чем закончилось действие.
type capWriter struct {
	http.ResponseWriter
	buf bytes.Buffer
}

func (c *capWriter) Write(b []byte) (int, error) {
	if room := 8192 - c.buf.Len(); room > 0 {
		if len(b) < room {
			room = len(b)
		}
		c.buf.Write(b[:room])
	}
	return c.ResponseWriter.Write(b)
}

func (c *capWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// actionLog — прослойка перед обработчиками: пишет действия в журнал.
func actionLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		title, ok := actionNames[r.URL.Path]
		if !ok || r.Method != http.MethodPost || r.Header.Get("X-Token") != token {
			next.ServeHTTP(w, r)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		r.Body = io.NopCloser(bytes.NewReader(body))
		var a actReq
		_ = json.Unmarshal(body, &a)
		who := ""
		if a.ID != "" {
			if p, err := getProfile(a.ID); err == nil {
				who = "«" + p.Name + "»: "
			}
		}
		cw := &capWriter{ResponseWriter: w}
		t0 := time.Now()
		next.ServeHTTP(cw, r)
		res := "OK"
		var out struct {
			Error     string `json:"error"`
			NeedForce bool   `json:"needForce"`
			Reason    string `json:"reason"`
		}
		if json.Unmarshal(cw.buf.Bytes(), &out) == nil || cw.buf.Len() >= 8192 {
			switch {
			case out.Error != "":
				res = "ОШИБКА: " + out.Error
			case out.NeedForce:
				res = "НЕ ВЫПОЛНЕНО: " + out.Reason
			}
		}
		desc := a.describe(r.URL.Path)
		if desc != "" {
			desc = " (" + desc + ")"
		}
		logLine(fmt.Sprintf("[Действие] %s%s%s — %s [%.1f с]", who, title, desc, res, time.Since(t0).Seconds()))
	})
}

// logTaskChunk пишет в журнал вывод задачи на сервере (обновление, удаление).
func logTaskChunk(role, task, chunk string) {
	name := taskNames[task]
	if name == "" {
		name = task
	}
	for _, l := range strings.Split(chunk, "\n") {
		if l = strings.TrimRight(l, " \t\r"); strings.TrimSpace(l) != "" {
			logLine(fmt.Sprintf("[%s] [%s] %s", role, name, l))
		}
	}
}

// Изменения состояния каскада (появились или ушли проблемы) — без повтора
// каждые 30 секунд одного и того же.
var (
	stateMu   sync.Mutex
	lastState = map[string]string{}
)

func logStateChange(p *Profile, problems []string) {
	s := strings.Join(problems, "; ")
	stateMu.Lock()
	prev, seen := lastState[p.ID]
	lastState[p.ID] = s
	stateMu.Unlock()
	if seen && prev == s || !seen && s == "" {
		return
	}
	if s == "" {
		logLine(fmt.Sprintf("[Состояние] «%s»: проблем нет, каскад работает", p.Name))
	} else {
		logLine(fmt.Sprintf("[Состояние] «%s»: %s", p.Name, s))
	}
}

func logPath() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "Documents", "AmneziaWG-Cascade")
	if st, err := os.Stat(filepath.Join(home, "Documents")); err != nil || !st.IsDir() {
		dir = filepath.Join(home, "AmneziaWG-Cascade")
	}
	return filepath.Join(dir, "log.txt")
}

// rotateLog: журнал больше 5 МБ переименовывается в log.old.txt (прежний старый удаляется).
func rotateLog(path string) {
	if st, err := os.Stat(path); err == nil && st.Size() > 5<<20 {
		old := strings.TrimSuffix(path, ".txt") + ".old.txt"
		_ = os.Remove(old)
		_ = os.Rename(path, old)
	}
}

func handleOpenLog(w http.ResponseWriter, r *http.Request) {
	p := logPath()
	if _, err := os.Stat(p); err != nil {
		writeJSON(w, map[string]string{"error": "журнал ещё пуст"})
		return
	}
	openLogFile(p)
	writeJSON(w, map[string]any{"ok": true})
}

var (
	finMu   sync.Mutex
	lastFin = map[string]time.Time{}
)

// logTaskFinish — итог задачи; один раз, даже если окно опросило её повторно.
func logTaskFinish(p *Profile, role, task string, rc int) {
	key := p.ID + "/" + role + "/" + task
	finMu.Lock()
	if t, ok := lastFin[key]; ok && time.Since(t) < 10*time.Minute {
		finMu.Unlock()
		return
	}
	lastFin[key] = time.Now()
	finMu.Unlock()
	name := taskNames[task]
	if name == "" {
		name = task
	}
	res := "успешно"
	if rc != 0 {
		res = fmt.Sprintf("с ошибкой (код %d)", rc)
	}
	logLine(fmt.Sprintf("[%s] [%s] «%s»: задача завершена %s", role, name, p.Name, res))
}
