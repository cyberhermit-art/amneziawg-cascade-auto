// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Job struct {
	in  JobInput
	hub *Hub
	ctx context.Context
}

func (j *Job) logf(src, f string, a ...any) { j.hub.Log(src, fmt.Sprintf(f, a...)) }

func (j *Job) Run() {
	res := &FinalResult{RUIP: j.in.RU.Host, EUIP: j.in.EU.Host}
	// Профиль сохраняем сразу: даже если установка прервётся, данные для
	// входа не придётся вводить заново.
	if p, perr := saveProfile(Profile{Name: j.in.ProfileName, RU: j.in.RU, EU: j.in.EU, RUSubnet: j.in.RUSubnet, EUSubnet: j.in.EUSubnet}); perr == nil {
		res.Profile = p.ID
		j.hub.Log("", "Данные серверов сохранены в профиль «"+p.Name+"».")
	} else {
		j.hub.Log("", "Не удалось сохранить профиль: "+perr.Error())
	}
	err := j.run(res)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			err = errors.New("остановлено пользователем. То, что уже запущено на сервере, доработает само — запустите программу ещё раз с теми же данными, и она продолжит с того же места")
		}
		res.Error = err.Error()
		j.hub.Log("", "ОШИБКА: "+err.Error())
	}
	j.hub.emit(Event{T: "finish", OK: err == nil, Result: res})
}

func (j *Job) stage(id string, f func() error) error {
	j.hub.Stage(id, "run", "")
	if err := f(); err != nil {
		j.hub.Stage(id, "err", err.Error())
		return err
	}
	j.hub.Stage(id, "ok", "")
	return nil
}

func (j *Job) run(res *FinalResult) error {
	ru := NewRemote(j.in.RU, func(s string) { j.hub.Log("RU", s) })
	eu := NewRemote(j.in.EU, func(s string) { j.hub.Log("EU", s) })
	defer ru.Close()
	defer eu.Close()

	// 0. Сразу проверяем доступ к обоим серверам — чтобы ошибку в пароле
	//    к EU узнать сейчас, а не через час.
	if err := j.stage("connect", func() error {
		if err := j.precheck(ru); err != nil {
			return err
		}
		return j.precheck(eu)
	}); err != nil {
		return err
	}

	// 1. Сервер RU: обновление, затем установка.
	if err := j.updateStage("ru-update", ru); err != nil {
		return err
	}
	if err := j.stage("ru-install", func() error { return j.installServer(ru, j.in.RUSubnet) }); err != nil {
		return err
	}

	// 2. Сервер EU: обновление, затем установка.
	if err := j.updateStage("eu-update", eu); err != nil {
		return err
	}
	if err := j.stage("eu-install", func() error { return j.installServer(eu, j.in.EUSubnet) }); err != nil {
		return err
	}

	// 3. Каскад и файлы подключения.
	if err := j.stage("cascade", func() error { return j.cascade(ru, eu) }); err != nil {
		return err
	}
	if err := j.stage("clients", func() error { return j.clients(ru, res) }); err != nil {
		return err
	}
	j.hub.Log("", "Готово! Каскад настроен.")
	return nil
}

func (j *Job) updateStage(id string, r *Remote) error {
	if j.in.SkipUpdate {
		j.hub.Stage(id, "skip", "")
		return nil
	}
	return j.stage(id, func() error { return j.updateServer(r) })
}

// ---------- общие помощники ----------

func (j *Job) sleep(d time.Duration) error {
	select {
	case <-j.ctx.Done():
		return j.ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// reconnect — ждём, пока сервер снова станет доступен по SSH.
func (j *Job) reconnect(r *Remote, why string) error {
	if why != "" {
		j.hub.Log(r.cfg.Role, why)
	}
	if err := r.Reconnect(j.ctx, 20*time.Minute); err != nil {
		return fmt.Errorf("сервер %s: %v", r.cfg.Role, err)
	}
	j.hub.Log(r.cfg.Role, "Сервер снова на связи.")
	return nil
}

// exec выполняет команду; при обрыве связи переподключается и повторяет.
// Использовать только для команд, которые безопасно повторить.
func (j *Job) exec(r *Remote, cmd string, stdin []byte, timeout time.Duration) (string, int, error) {
	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		out, code, err := r.Output(cmd, stdin, timeout)
		if err == nil {
			return out, code, nil
		}
		lastErr = err
		if !errors.Is(err, errConnLost) {
			return out, code, err
		}
		if err := j.reconnect(r, "Связь с сервером прервалась — переподключаюсь и повторяю…"); err != nil {
			return "", -1, err
		}
	}
	return "", -1, fmt.Errorf("сервер %s: %v", r.cfg.Role, lastErr)
}

func (j *Job) upload(r *Remote, path string, data []byte, mode string) error {
	out, code, err := j.exec(r, fmt.Sprintf("umask 077; cat > %s.tmp && chmod %s %s.tmp && mv -f %s.tmp %s", shq(path), mode, shq(path), shq(path), shq(path)), data, 2*time.Minute)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("сервер %s: не удалось записать %s: %s", r.cfg.Role, path, strings.TrimSpace(out))
	}
	return nil
}

func (j *Job) precheck(r *Remote) error {
	role := r.cfg.Role
	j.logf(role, "Подключаюсь к %s…", r.cfg.addr())
	if err := r.Connect(); err != nil {
		if isFatalSSH(err) {
			return fmt.Errorf("сервер %s (%s): %v", role, r.cfg.Host, err)
		}
		// Сервер может как раз перезагружаться (если программу перезапустили
		// посреди установки) — даём ему несколько минут.
		j.logf(role, "%v. Пробую ещё раз в течение 5 минут…", err)
		if err := r.Reconnect(j.ctx, 5*time.Minute); err != nil {
			return fmt.Errorf("сервер %s (%s): %v", role, r.cfg.Host, err)
		}
	}
	out, _, err := j.exec(r, `echo "@@ID $(id -u)"; . /etc/os-release 2>/dev/null; echo "@@OS $PRETTY_NAME"; echo "@@ARCH $(uname -m)"; echo "@@SYSD $(command -v systemd-run)"`, nil, 60*time.Second)
	if err != nil {
		return fmt.Errorf("сервер %s: %v", role, err)
	}
	get := func(k string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "@@"+k+" ") {
				return strings.TrimSpace(strings.TrimPrefix(l, "@@"+k+" "))
			}
		}
		return ""
	}
	if get("ID") != "0" {
		return fmt.Errorf("сервер %s: нужен вход под root", role)
	}
	osName, arch := get("OS"), get("ARCH")
	j.logf(role, "Подключено. Система: %s (%s)", osName, arch)
	low := strings.ToLower(osName)
	if !(strings.Contains(low, "ubuntu") || strings.Contains(low, "debian")) {
		return fmt.Errorf("сервер %s: система %q не поддерживается — нужна Ubuntu 24.04 / 26.04 или Debian 13", role, osName)
	}
	if get("SYSD") == "" {
		return fmt.Errorf("сервер %s: на сервере нет systemd (похоже на контейнер OpenVZ/LXC) — AmneziaWG на таком VPS не установить", role)
	}
	return nil
}

// ---------- наблюдение за фоновой задачей на сервере ----------

type probeResult struct {
	running, done, rebootPending bool
	bootID                       string
	size                         int64
	chunk                        string
}

// probe одной короткой командой узнаёт состояние задачи и забирает новые строки её лога.
func (j *Job) probe(r *Remote, logPath, runningTest, doneTest string, offset int64) (*probeResult, error) {
	cmd := fmt.Sprintf(`L=%s; O=%d
R=0; if %s; then R=1; fi
D=0; if %s; then D=1; fi
P=0; [ -f /run/reboot-required ] && P=1
B=$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)
S=$(stat -c %%s "$L" 2>/dev/null || echo 0)
echo "@@ $R $D $P $B $S"
if [ "$S" -gt "$O" ]; then F=$O; [ $((S-O)) -gt 200000 ] && F=$((S-200000)); tail -c +$((F+1)) "$L" | head -c $((S-F)); fi`,
		shq(logPath), offset, runningTest, doneTest)
	out, _, err := r.Output(cmd, nil, 90*time.Second)
	if err != nil {
		return nil, err
	}
	nl := strings.IndexByte(out, '\n')
	if nl < 0 || !strings.HasPrefix(out, "@@ ") {
		return nil, fmt.Errorf("неожиданный ответ сервера: %.200s", out)
	}
	f := strings.Fields(out[:nl])
	if len(f) < 6 {
		return nil, fmt.Errorf("неожиданный ответ сервера: %.200s", out)
	}
	p := &probeResult{running: f[1] == "1", done: f[2] == "1", rebootPending: f[3] == "1", bootID: f[4], chunk: out[nl+1:]}
	p.size, _ = strconv.ParseInt(f[5], 10, 64)
	return p, nil
}

// ---------- шаг «обновление системы» ----------

const (
	updRunning = `systemctl is-active --quiet awgc-update`
	updDone    = `[ -f ` + updateDone + ` ]`
)

func (j *Job) updateServer(r *Remote) error {
	role := r.cfg.Role
	p, err := j.probe(r, updateLog, updRunning, updDone, 1<<62)
	if err != nil {
		if !errors.Is(err, errConnLost) {
			return fmt.Errorf("сервер %s: %v", role, err)
		}
		if err := j.reconnect(r, "Связь прервалась — переподключаюсь…"); err != nil {
			return err
		}
		if p, err = j.probe(r, updateLog, updRunning, updDone, 1<<62); err != nil {
			return fmt.Errorf("сервер %s: %v", role, err)
		}
	}
	if p.done && !p.running && !p.rebootPending {
		j.logf(role, "Система уже обновлена — пропускаю этот шаг.")
		return nil
	}
	offset := p.size

	launch := func() error {
		if err := j.upload(r, updatePath, updateScript, "700"); err != nil {
			return err
		}
		out, code, err := j.exec(r, fmt.Sprintf(`systemctl reset-failed awgc-update >/dev/null 2>&1
systemd-run --unit=awgc-update --collect --quiet /bin/bash -c 'bash %s >> %s 2>&1'`, updatePath, updateLog), nil, time.Minute)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("сервер %s: не удалось запустить обновление: %s", role, strings.TrimSpace(out))
		}
		return nil
	}

	switch {
	case p.running:
		j.logf(role, "Обновление системы уже идёт (с прошлого запуска) — продолжаю следить.")
	case p.done && p.rebootPending:
		// обновление прошло, но перезагрузка почему-то не случилась
	default:
		j.logf(role, "Обновляю систему сервера (apt update + upgrade). На медленном канале это может занять до часа — следите за логом ниже.")
		if err := launch(); err != nil {
			return err
		}
	}

	deadline := time.Now().Add(3 * time.Hour)
	fails := 0
	var pendingSince time.Time
	rebootRequested := false
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("сервер %s: обновление идёт дольше 3 часов — проверьте сервер (лог: %s)", role, updateLog)
		}
		if err := j.sleep(8 * time.Second); err != nil {
			return err
		}
		p, err := j.probe(r, updateLog, updRunning, updDone, offset)
		if err != nil {
			if errors.Is(err, errConnLost) {
				if err := j.reconnect(r, "Связь с сервером пропала (перезагрузка или перезапуск SSH) — жду…"); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("сервер %s: %v", role, err)
		}
		if p.size < offset {
			offset = 0
		}
		if p.chunk != "" {
			j.hub.Log(role, p.chunk)
		}
		offset = p.size

		if p.running {
			continue
		}
		if p.done {
			if !p.rebootPending {
				j.logf(role, "Система обновлена.")
				return nil
			}
			// Ждём перезагрузку, которую запланировал скрипт; если её нет — делаем сами.
			if pendingSince.IsZero() {
				pendingSince = time.Now()
			}
			if !rebootRequested && time.Since(pendingSince) > 90*time.Second {
				j.logf(role, "Перезагружаю сервер, чтобы применить обновления…")
				_, _, _ = r.Output(`systemd-run --quiet --on-active=3 /bin/systemctl reboot`, nil, 30*time.Second)
				rebootRequested = true
			}
			continue
		}
		// Не работает и не завершилось — значит упало.
		fails++
		if fails >= 3 {
			return fmt.Errorf("сервер %s: обновление системы не удалось — причина в логе выше. Можно поставить галочку «Пропустить обновление» и попробовать снова", role)
		}
		j.logf(role, "Обновление прервалось. Подожду минуту и попробую ещё раз…")
		if err := j.sleep(time.Minute); err != nil {
			return err
		}
		if err := launch(); err != nil {
			return err
		}
	}
}

// ---------- шаг «установка AmneziaWG» ----------

const (
	instRunning = `pgrep -f '[i]nstall_amneziawg.sh' >/dev/null 2>&1 || systemctl is-active --quiet awgc-install`
	instDone    = `[ ! -f /root/awg/setup_state ] && [ -f /etc/amnezia/amneziawg/awg0.conf ] && [ -f /root/awg/manage_amneziawg.sh ] && systemctl is-active --quiet awg-quick@awg0`
)

func (j *Job) installServer(r *Remote, subnet string) error {
	role := r.cfg.Role
	if err := j.ensure(r); err != nil {
		return err
	}
	p, err := j.probe(r, installLog, instRunning, instDone, 1<<62)
	if err != nil {
		return fmt.Errorf("сервер %s: %v", role, err)
	}
	if p.done && !p.running {
		j.logf(role, "AmneziaWG уже установлен — пропускаю установку.")
		return nil
	}
	offset := p.size

	launchBoot := p.bootID
	launch := func() error {
		out, code, err := j.exec(r, fmt.Sprintf(`systemctl reset-failed awgc-install >/dev/null 2>&1
systemd-run --unit=awgc-install --collect --quiet --setenv=HOME=/root /bin/bash -c 'cd /root && bash %s --yes --disallow-ipv6 --route-all --subnet=%s >> %s 2>&1'
cat /proc/sys/kernel/random/boot_id`, installerPath, subnet, installLog), nil, time.Minute)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("сервер %s: не удалось запустить установщик: %s", role, strings.TrimSpace(out))
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		launchBoot = strings.TrimSpace(lines[len(lines)-1])
		return nil
	}

	// Программу могли перезапустить ровно в момент, когда установщик отправил
	// сервер в перезагрузку, — тогда сначала дожидаемся её.
	seenReboot := false
	var rebootWaitSince time.Time
	if !p.running && p.size > 0 {
		from := p.size - 4000
		if from < 0 {
			from = 0
		}
		if t, err := j.probe(r, installLog, instRunning, instDone, from); err == nil {
			i := strings.LastIndex(t.chunk, "Инициирована перезагрузка")
			if i >= 0 && i > strings.LastIndex(t.chunk, "Выполнение шага") {
				// Перезагрузка уже была, если сервер загрузился позже последней записи в лог.
				out, _, _ := j.exec(r, fmt.Sprintf(`[ $(stat -c %%Y %s) -lt $(( $(date +%%s) - $(cut -d. -f1 /proc/uptime) )) ] && echo REBOOTED`, installLog), nil, time.Minute)
				if !strings.Contains(out, "REBOOTED") {
					seenReboot, rebootWaitSince = true, time.Now()
				}
			}
		}
	}

	switch {
	case p.running:
		j.logf(role, "Установка уже идёт (с прошлого запуска) — продолжаю следить.")
	case seenReboot:
		j.logf(role, "Установщик только что отправил сервер в перезагрузку — жду её, потом продолжу.")
	default:
		if err := j.fetchInstaller(r); err != nil {
			return err
		}
		j.logf(role, "Запускаю установщик AmneziaWG (подсеть %s). Сервер сам перезагрузится 1–2 раза — это нормально.", subnet)
		if err := launch(); err != nil {
			return err
		}
	}

	deadline := time.Now().Add(2 * time.Hour)
	sameBootFails, launches := 0, 1
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("сервер %s: установка идёт дольше 2 часов — проверьте сервер вручную (лог: %s)", role, installLog)
		}
		if err := j.sleep(8 * time.Second); err != nil {
			return err
		}
		p, err := j.probe(r, installLog, instRunning, instDone, offset)
		if err != nil {
			if errors.Is(err, errConnLost) {
				if err := j.reconnect(r, "Связь с сервером пропала — видимо, он перезагружается. Жду…"); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("сервер %s: %v", role, err)
		}
		if p.size < offset {
			offset = 0
		}
		if p.chunk != "" {
			j.hub.Log(role, p.chunk)
			if strings.Contains(p.chunk, "Инициирована перезагрузка") {
				seenReboot, rebootWaitSince = true, time.Now()
			}
		}
		offset = p.size

		if p.running {
			continue
		}
		if p.done {
			j.logf(role, "AmneziaWG установлен.")
			return nil
		}
		if p.bootID == launchBoot {
			if seenReboot && time.Since(rebootWaitSince) < 3*time.Minute {
				continue // сервер вот-вот уйдёт в перезагрузку
			}
			sameBootFails++
			if sameBootFails >= 4 {
				return fmt.Errorf("сервер %s: установщик остановился с ошибкой — последние строки лога выше", role)
			}
			// Частая временная причина — после загрузки система сама ставит
			// обновления и держит блокировку apt. Даём ей закончить.
			j.logf(role, "Установщик остановился. Подожду минуту и попробую ещё раз…")
			if err := j.sleep(time.Minute); err != nil {
				return err
			}
		} else {
			sameBootFails = 0
		}
		launches++
		if launches > 10 {
			return fmt.Errorf("сервер %s: установщик перезапускался слишком много раз — смотрите лог выше", role)
		}
		seenReboot = false
		j.logf(role, "Продолжаю установку после перезагрузки (запуск №%d)…", launches)
		if err := launch(); err != nil {
			return err
		}
	}
}

// ensure проверяет, что подключение живое (сервер мог долго простаивать).
func (j *Job) ensure(r *Remote) error {
	_, _, err := j.exec(r, "true", nil, 30*time.Second)
	return err
}

func (j *Job) fetchInstaller(r *Remote) error {
	role := r.cfg.Role
	j.logf(role, "Скачиваю установщик на сервер…")
	cmd := fmt.Sprintf(`cd /root && { wget -q --tries=2 -T 30 -O %[1]s.new %[2]s || curl -fsSL --retry 1 --connect-timeout 20 --max-time 90 -o %[1]s.new %[2]s; } && head -1 %[1]s.new | grep -q bash && mv -f %[1]s.new %[1]s`, installerPath, installerURL)
	out, code, err := j.exec(r, cmd, nil, 5*time.Minute)
	if err == nil && code == 0 {
		return nil
	}
	j.logf(role, "С сервера скачать не вышло (%s) — скачиваю через этот компьютер.", strings.TrimSpace(out))
	data, herr := httpGet(installerURL)
	if herr != nil {
		return fmt.Errorf("не удалось скачать установщик ни на сервере, ни на этом компьютере: %v", herr)
	}
	return j.upload(r, installerPath, data, "700")
}

func httpGet(url string) ([]byte, error) {
	c := &http.Client{Timeout: 120 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 20<<20))
}

// ---------- шаг «каскад» ----------

func (j *Job) cascade(ru, eu *Remote) error {
	j.hub.Log("EU", "Создаю на сервере EU подключение для сервера RU (клиент ru_host)…")
	out, code, err := j.exec(eu, `[ -f /root/awg/ru_host.conf ] || bash /root/awg/manage_amneziawg.sh add ru_host >/tmp/ru_host_add.log 2>&1 || { cat /tmp/ru_host_add.log; exit 1; }; cat /root/awg/ru_host.conf`, nil, 5*time.Minute)
	if err != nil {
		return err
	}
	if code != 0 || !strings.Contains(out, "[Peer]") {
		j.hub.Log("EU", out)
		return errors.New("сервер EU: не удалось создать клиента ru_host")
	}

	j.hub.Log("RU", "Передаю настройки на сервер RU и запускаю настройку каскада…")
	if err := j.upload(ru, "/root/ru_host.conf", []byte(out), "600"); err != nil {
		return err
	}
	if err := j.upload(ru, "/root/cascade-setup.sh", cascadeScript, "700"); err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		code, err = ru.Stream("bash /root/cascade-setup.sh /root/ru_host.conf", func(l string) { j.hub.Log("RU", l) }, 20*time.Minute)
		if err == nil {
			break
		}
		if !errors.Is(err, errConnLost) || attempt >= 3 {
			return fmt.Errorf("сервер RU: %v", err)
		}
		// скрипт безопасно запускать повторно
		if err := j.reconnect(ru, "Связь прервалась во время настройки каскада — переподключаюсь и запускаю ещё раз…"); err != nil {
			return err
		}
	}
	if code != 0 {
		return errors.New("настройка каскада завершилась с ошибкой — причина в логе выше")
	}
	return nil
}

// ---------- шаг «файлы подключения» ----------

func (j *Job) clients(ru *Remote, res *FinalResult) error {
	for _, n := range extraNames(j.in.ExtraClients) {
		j.logf("RU", "Создаю клиента %s…", n)
		out, code, err := j.exec(ru, fmt.Sprintf(`[ -f /root/awg/%[1]s.conf ] || bash /root/awg/manage_amneziawg.sh add %[1]s`, n), nil, 5*time.Minute)
		if err != nil {
			return err
		}
		if code != 0 {
			j.hub.Log("RU", out)
			return fmt.Errorf("не удалось создать клиента %s", n)
		}
	}
	out, _, err := j.exec(ru, `cd /root/awg 2>/dev/null && for f in *.conf; do [ -f "$f" ] || continue; n=${f%.conf}; echo "@@NAME $n"; echo "@@CONF"; base64 -w0 "$f"; echo; if [ -f "$n.vpnuri" ]; then echo "@@URI"; base64 -w0 "$n.vpnuri"; echo; fi; if [ -f "$n.png" ]; then echo "@@PNG"; base64 -w0 "$n.png"; echo; fi; done`, nil, 3*time.Minute)
	if err != nil {
		return err
	}
	var cur *ClientFile
	var list []ClientFile
	lines := strings.Split(out, "\n")
	for i := 0; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		next := func() []byte {
			if i+1 < len(lines) {
				i++
				b, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[i]))
				return b
			}
			return nil
		}
		switch {
		case strings.HasPrefix(l, "@@NAME "):
			if cur != nil {
				list = append(list, *cur)
			}
			cur = &ClientFile{Name: strings.TrimPrefix(l, "@@NAME ")}
		case l == "@@CONF" && cur != nil:
			cur.Conf = string(next())
		case l == "@@URI" && cur != nil:
			cur.VpnURI = strings.TrimSpace(string(next()))
		case l == "@@PNG" && cur != nil:
			cur.PNG = base64.StdEncoding.EncodeToString(next())
		}
	}
	if cur != nil {
		list = append(list, *cur)
	}
	sort.Slice(list, func(a, b int) bool { return list[a].Name < list[b].Name })
	if len(list) == 0 {
		return errors.New("на сервере RU не найдено ни одного клиента")
	}

	dir := outputDir(j.in.RU.Host)
	if err := os.MkdirAll(dir, 0o700); err == nil {
		for _, c := range list {
			_ = os.WriteFile(filepath.Join(dir, c.Name+".conf"), []byte(c.Conf), 0o600)
			if c.VpnURI != "" {
				_ = os.WriteFile(filepath.Join(dir, c.Name+".vpnuri.txt"), []byte(c.VpnURI+"\n"), 0o600)
			}
			if c.PNG != "" {
				b, _ := base64.StdEncoding.DecodeString(c.PNG)
				_ = os.WriteFile(filepath.Join(dir, c.Name+"-qr.png"), b, 0o600)
			}
		}
		res.Folder = dir
		j.logf("", "Файлы подключений сохранены в папку: %s", dir)
	}
	res.Clients = list
	return nil
}

func outputDir(host string) string {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, "Documents")
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		base = home
	}
	safe := strings.Map(func(r rune) rune {
		if r == ':' || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, host)
	return filepath.Join(base, "AmneziaWG-Cascade", safe)
}
