// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

// Долгие задачи обслуживания на серверах: обновление пакетов и удаление VPN.
// Как и при установке, задача запускается на сервере отдельной службой
// systemd-run, а программа только читает её журнал — поэтому обрыв связи,
// перезапуск SSH или закрытие программы задачу не прерывают.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type taskReq struct {
	ID     string `json:"id"`
	Target string `json:"target"` // ru | eu
	Task   string `json:"task"`   // update | uninstall
	Reboot bool   `json:"reboot"`
	Offset int64  `json:"offset"`
}

func taskPaths(task string) (unit, log, rc string) {
	return "awgc-task-" + task, "/var/log/awgc-" + task + ".log", "/var/log/awgc-" + task + ".rc"
}

func roleOf(target string) (string, error) {
	switch strings.ToLower(target) {
	case "ru":
		return "RU", nil
	case "eu":
		return "EU", nil
	}
	return "", errors.New("неизвестный сервер")
}

func validTask(t string) bool { return t == "update" || t == "uninstall" }

// serverConn — подключение к нужному серверу профиля с проверкой, что есть данные для входа.
func serverConn(p *Profile, role string) (*conn, error) {
	s := p.RU
	if role == "EU" {
		s = p.EU
	}
	if strings.TrimSpace(s.Host) == "" || (s.Password == "" && s.Key == "") {
		return nil, fmt.Errorf("в профиле нет данных для входа на сервер %s — добавьте их во вкладке «Профиль»", role)
	}
	return getConn(p, role), nil
}

func (c *conn) lockedRun(cmd string, stdin []byte, timeout time.Duration) (string, string, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.run(cmd, stdin, timeout)
}

func (c *conn) upload(path string, data []byte, mode string) error {
	out, se, code, err := c.lockedRun(fmt.Sprintf("umask 077; cat > %[1]s.tmp && chmod %[2]s %[1]s.tmp && mv -f %[1]s.tmp %[1]s", shq(path), mode), data, 2*time.Minute)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("не удалось записать %s: %s", path, tail(se+out, 2))
	}
	return nil
}

// ensureInstaller кладёт на сервер install_amneziawg.sh (нужен для удаления).
func (c *conn) ensureInstaller() error {
	cmd := fmt.Sprintf(`[ -s %[1]s ] && head -1 %[1]s | grep -q bash && exit 0
cd /root && { wget -q --tries=2 -T 30 -O %[1]s.new %[2]s || curl -fsSL --retry 1 --connect-timeout 20 --max-time 90 -o %[1]s.new %[2]s; } && head -1 %[1]s.new | grep -q bash && mv -f %[1]s.new %[1]s`, installerPath, installerURL)
	if _, _, code, err := c.lockedRun(cmd, nil, 5*time.Minute); err == nil && code == 0 {
		return nil
	}
	data, err := httpGet(installerURL)
	if err != nil {
		return fmt.Errorf("не удалось скачать установщик AmneziaWG ни на сервере, ни на этом компьютере: %v", err)
	}
	return c.upload(installerPath, data, "700")
}

const uninstallRU = `set +e
echo "== Отключаю каскад (туннель до EU, маршрутизация, автозапуск)"
bash /root/cascade-setup.sh --remove 2>&1 | grep -v "удалите клиента"
echo "== Убираю сбор статистики"
rm -f /etc/cron.d/awgc-stats
echo "== Удаляю AmneziaWG (установщик сохранит свою резервную копию в /root)"
bash /root/install_amneziawg.sh --uninstall --yes
rc=$?
rm -f /root/awgc-* /root/.awgc-* /root/cascade-setup.sh /root/ru_host.conf /root/awg-cascade-install.log /root/install_amneziawg.sh
[ $rc -eq 0 ] && echo "== VPN с сервера удалён" || echo "ОШИБКА: установщик завершился с кодом $rc"
exit $rc`

const uninstallEU = `set +e
echo "== Удаляю AmneziaWG (установщик сохранит свою резервную копию в /root)"
rm -f /etc/cron.d/awgc-stats
bash /root/install_amneziawg.sh --uninstall --yes
rc=$?
rm -f /root/awgc-* /root/.awgc-* /root/awg-cascade-install.log /root/install_amneziawg.sh
[ $rc -eq 0 ] && echo "== VPN с сервера удалён" || echo "ОШИБКА: установщик завершился с кодом $rc"
exit $rc`

func handleTaskStart(m *mctx, tr taskReq) (any, error) {
	role, err := roleOf(tr.Target)
	if err != nil || !validTask(tr.Task) {
		return nil, errors.New("неверная задача")
	}
	c, err := serverConn(m.p, role)
	if err != nil {
		return nil, err
	}
	unit, log, rc := taskPaths(tr.Task)

	// Уже идёт (например, запущена в прошлый раз) — просто подключаемся к журналу.
	if out, _, _, err := c.lockedRun("systemctl is-active --quiet "+unit+" && echo RUN", nil, 30*time.Second); err != nil {
		return nil, fmt.Errorf("сервер %s: %v", role, err)
	} else if strings.Contains(out, "RUN") {
		return map[string]any{"ok": true, "attached": true}, nil
	}

	var body string
	switch tr.Task {
	case "update":
		if err := c.upload(updatePath, updateScript, "700"); err != nil {
			return nil, fmt.Errorf("сервер %s: %v", role, err)
		}
		nr := "1"
		if tr.Reboot {
			nr = "0"
		}
		body = fmt.Sprintf("AWGC_NO_REBOOT=%s AWGC_DONE=/var/log/awgc-update.done bash %s", nr, updatePath)
	case "uninstall":
		if err := c.ensureInstaller(); err != nil {
			return nil, fmt.Errorf("сервер %s: %v", role, err)
		}
		script := uninstallEU
		if role == "RU" {
			if err := c.upload("/root/cascade-setup.sh", cascadeScript, "700"); err != nil {
				return nil, fmt.Errorf("сервер RU: %v", err)
			}
			script = uninstallRU
		}
		if err := c.upload("/var/lib/awgc-uninstall.sh", []byte(script), "700"); err != nil {
			return nil, fmt.Errorf("сервер %s: %v", role, err)
		}
		body = "bash /var/lib/awgc-uninstall.sh"
	}
	cmd := fmt.Sprintf(`rm -f %[3]s; : > %[2]s
systemctl reset-failed %[1]s >/dev/null 2>&1
systemd-run --unit=%[1]s --collect --quiet /bin/bash -c '%[4]s >> %[2]s 2>&1; echo $? > %[3]s'`, unit, log, rc, body)
	out, se, code, err := c.lockedRun(cmd, nil, time.Minute)
	if err != nil {
		return nil, fmt.Errorf("сервер %s: %v", role, err)
	}
	if code != 0 {
		return nil, fmt.Errorf("сервер %s: не удалось запустить задачу: %s", role, tail(se+out, 3))
	}
	if tr.Task == "uninstall" {
		// подключения к серверу после удаления больше не понадобятся в прежнем виде
		c.mu.Lock()
		c.prepOK = false
		c.mu.Unlock()
	}
	return map[string]any{"ok": true}, nil
}

type taskState struct {
	Running       bool   `json:"running"`
	Finished      bool   `json:"finished"`
	RC            int    `json:"rc"`
	RebootPending bool   `json:"rebootPending"`
	Chunk         string `json:"chunk"`
	Size          int64  `json:"size"`
	Offline       bool   `json:"offline"` // сервер сейчас недоступен (например, перезагружается)
	Note          string `json:"note,omitempty"`
}

func handleTaskPoll(m *mctx, tr taskReq) (any, error) {
	role, err := roleOf(tr.Target)
	if err != nil || !validTask(tr.Task) {
		return nil, errors.New("неверная задача")
	}
	c, err := serverConn(m.p, role)
	if err != nil {
		return nil, err
	}
	unit, log, rc := taskPaths(tr.Task)
	cmd := fmt.Sprintf(`L=%s; O=%d
R=0; systemctl is-active --quiet %s && R=1
RC=$(cat %s 2>/dev/null); [ -n "$RC" ] || RC=-
P=0; [ -f /run/reboot-required ] && P=1
S=$(stat -c %%s "$L" 2>/dev/null || echo 0)
echo "@@ $R $RC $P $S"
if [ "$S" -gt "$O" ]; then F=$O; [ $((S-O)) -gt 200000 ] && F=$((S-200000)); tail -c +$((F+1)) "$L" | head -c $((S-F)); fi`,
		log, tr.Offset, unit, rc)
	out, _, _, err := c.lockedRun(cmd, nil, 60*time.Second)
	if err != nil {
		return &taskState{Offline: true, Size: tr.Offset, RC: -1, Note: err.Error()}, nil
	}
	nl := strings.IndexByte(out, '\n')
	if nl < 0 || !strings.HasPrefix(out, "@@ ") {
		return nil, fmt.Errorf("неожиданный ответ сервера")
	}
	f := strings.Fields(out[:nl])
	if len(f) < 5 {
		return nil, fmt.Errorf("неожиданный ответ сервера")
	}
	st := &taskState{Running: f[1] == "1", RC: -1, RebootPending: f[3] == "1", Chunk: clean(out[nl+1:])}
	st.Size, _ = strconv.ParseInt(f[4], 10, 64)
	if f[2] != "-" {
		st.RC, _ = strconv.Atoi(f[2])
		st.Finished = !st.Running
	}
	if st.Size < tr.Offset { // журнал начали заново
		st.Chunk = ""
	}
	// Файл кода завершения мог не появиться, если служба была убита (перезагрузка).
	if !st.Running && f[2] == "-" && st.Size > 0 {
		st.Finished = true
		if tr.Task == "update" && strings.Contains(out, "перезагружаю сервер") {
			st.RC = 0
		}
	}
	return st, nil
}

func handleServerReboot(m *mctx, tr taskReq) (any, error) {
	role, err := roleOf(tr.Target)
	if err != nil {
		return nil, err
	}
	c, err := serverConn(m.p, role)
	if err != nil {
		return nil, err
	}
	_, se, code, err := c.lockedRun(`systemd-run --quiet --on-active=2 /bin/systemctl reboot`, nil, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("сервер %s: %v", role, err)
	}
	if code != 0 {
		return nil, fmt.Errorf("сервер %s: не удалось перезагрузить: %s", role, tail(se, 3))
	}
	c.r.Close()
	return map[string]any{"ok": true}, nil
}

// tapi — как mapi, но для запросов задач обслуживания.
func tapi(f func(m *mctx, tr taskReq) (any, error)) func(w http.ResponseWriter, r *http.Request) {
	return api(func(w http.ResponseWriter, r *http.Request) {
		var tr taskReq
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&tr); err != nil {
			writeJSON(w, map[string]string{"error": "неверный запрос"})
			return
		}
		p, err := getProfile(tr.ID)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		res, err := f(&mctx{p: p}, tr)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, res)
	})
}

func registerTasks(mux *http.ServeMux) {
	mux.HandleFunc("/api/m/task-start", tapi(handleTaskStart))
	mux.HandleFunc("/api/m/task-poll", tapi(handleTaskPoll))
	mux.HandleFunc("/api/m/server-reboot", tapi(handleServerReboot))
}
