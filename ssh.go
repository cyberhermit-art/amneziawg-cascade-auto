// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// ServerCfg — то, что пользователь ввёл в форму для одного сервера.
type ServerCfg struct {
	Role     string `json:"-"` // "RU" или "EU"
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	Key      string `json:"key"`     // содержимое приватного ключа (необязательно)
	KeyPass  string `json:"keyPass"` // пароль ключа (необязательно)
}

func (s ServerCfg) addr() string {
	p := s.Port
	if p == 0 {
		p = 22
	}
	return net.JoinHostPort(strings.TrimSpace(s.Host), strconv.Itoa(p))
}

// Remote — SSH-подключение к серверу, которое умеет переподключаться
// (сервер дважды перезагружается во время установки).
type Remote struct {
	cfg ServerCfg
	log func(string)

	mu      sync.Mutex
	client  *ssh.Client
	hostKey string // отпечаток ключа сервера, запомненный при первом подключении
	stopKA  chan struct{}
}

func NewRemote(cfg ServerCfg, log func(string)) *Remote {
	return &Remote{cfg: cfg, log: log}
}

func (r *Remote) sshConfig() (*ssh.ClientConfig, error) {
	var auths []ssh.AuthMethod
	if strings.TrimSpace(r.cfg.Key) != "" {
		var signer ssh.Signer
		var err error
		if r.cfg.KeyPass != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(r.cfg.Key), []byte(r.cfg.KeyPass))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(r.cfg.Key))
		}
		if err != nil {
			return nil, fmt.Errorf("не удалось прочитать SSH-ключ: %v", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if r.cfg.Password != "" {
		pw := r.cfg.Password
		auths = append(auths, ssh.Password(pw))
		auths = append(auths, ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			ans := make([]string, len(qs))
			for i := range qs {
				ans[i] = pw
			}
			return ans, nil
		}))
	}
	if len(auths) == 0 {
		return nil, errors.New("не указан ни пароль, ни ключ")
	}
	user := strings.TrimSpace(r.cfg.User)
	if user == "" {
		user = "root"
	}
	return &ssh.ClientConfig{
		User:    user,
		Auth:    auths,
		Timeout: 15 * time.Second,
		// Доверие при первом подключении: запоминаем ключ сервера и после
		// перезагрузок требуем тот же самый (защита от подмены).
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			sum := sha256.Sum256(key.Marshal())
			fp := "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.hostKey == "" {
				r.hostKey = fp
				r.log("Ключ сервера: " + fp)
				return nil
			}
			if r.hostKey != fp {
				return fmt.Errorf("ключ сервера изменился (%s, ожидался %s) — подключение остановлено из соображений безопасности", fp, r.hostKey)
			}
			return nil
		},
	}, nil
}

// Connect подключается один раз.
func (r *Remote) Connect() error {
	cfg, err := r.sshConfig()
	if err != nil {
		return err
	}
	c, err := ssh.Dial("tcp", r.cfg.addr(), cfg)
	if err != nil {
		return humanSSHError(err)
	}
	r.mu.Lock()
	if r.client != nil {
		r.client.Close()
	}
	if r.stopKA != nil {
		close(r.stopKA)
	}
	r.client = c
	stop := make(chan struct{})
	r.stopKA = stop
	r.mu.Unlock()
	// keepalive: без него обрыв при перезагрузке замечался бы минутами
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				done := make(chan error, 1)
				go func() { _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); done <- err }()
				select {
				case err := <-done:
					if err != nil {
						c.Close()
						return
					}
				case <-time.After(20 * time.Second):
					c.Close()
					return
				}
			}
		}
	}()
	return nil
}

// Reconnect пытается подключиться до истечения maxWait.
func (r *Remote) Reconnect(ctx context.Context, maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	var last error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := r.Connect(); err == nil {
			return nil
		} else {
			last = err
			// Неверный пароль или подмена ключа — ждать бессмысленно
			if isFatalSSH(err) {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return fmt.Errorf("сервер не отвечает %d мин: %v", int(maxWait.Minutes()), last)
}

func (r *Remote) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopKA != nil {
		close(r.stopKA)
		r.stopKA = nil
	}
	if r.client != nil {
		r.client.Close()
		r.client = nil
	}
}

var errConnLost = errors.New("соединение потеряно")

// errTimeout считается разновидностью потери связи (errors.Is(errTimeout, errConnLost) == true).
var errTimeout = fmt.Errorf("%w: сервер не ответил вовремя", errConnLost)

func (r *Remote) dropClient() {
	r.mu.Lock()
	c := r.client
	r.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

func (r *Remote) session() (*ssh.Session, error) {
	r.mu.Lock()
	c := r.client
	r.mu.Unlock()
	if c == nil {
		return nil, errConnLost
	}
	s, err := c.NewSession()
	if err != nil {
		return nil, errConnLost
	}
	return s, nil
}

// Output выполняет команду и возвращает её вывод (stdout+stderr) и код выхода.
func (r *Remote) Output(cmd string, stdin []byte, timeout time.Duration) (string, int, error) {
	s, err := r.session()
	if err != nil {
		return "", -1, err
	}
	defer s.Close()
	buf := &lockedBuf{}
	s.Stdout = buf
	s.Stderr = buf
	if stdin != nil {
		s.Stdin = bytes.NewReader(stdin)
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(cmd) }()
	select {
	case err = <-done:
	case <-time.After(timeout):
		// Сервер не ответил вовремя: скорее всего он перегружен или связь
		// «подвисла». Рвём подключение — вызывающий код переподключится.
		s.Close()
		r.dropClient()
		return buf.String(), -1, errTimeout
	}
	return buf.String(), exitCode(err), connErr(err)
}

// Stream выполняет команду и построчно передаёт вывод в onLine.
func (r *Remote) Stream(cmd string, onLine func(string), timeout time.Duration) (int, error) {
	s, err := r.session()
	if err != nil {
		return -1, err
	}
	defer s.Close()
	pr, pw := io.Pipe()
	s.Stdout = pw
	s.Stderr = pw
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			onLine(sc.Text())
		}
	}()
	done := make(chan error, 1)
	go func() { done <- s.Run(cmd) }()
	select {
	case err = <-done:
	case <-time.After(timeout):
		s.Close()
		pw.Close()
		return -1, errors.New("команда выполнялась слишком долго")
	}
	pw.Close()
	time.Sleep(100 * time.Millisecond)
	return exitCode(err), connErr(err)
}

// Upload записывает файл на сервер.
func (r *Remote) Upload(path string, data []byte, mode string) error {
	out, code, err := r.Output(fmt.Sprintf("umask 077; cat > %s && chmod %s %s", shq(path), mode, shq(path)), data, 60*time.Second)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("не удалось записать %s: %s", path, strings.TrimSpace(out))
	}
	return nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		return ee.ExitStatus()
	}
	return -1
}

// connErr отделяет «команда завершилась с ошибкой» (не ошибка транспорта) от обрыва связи.
func connErr(err error) error {
	if err == nil {
		return nil
	}
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		return nil
	}
	var em *ssh.ExitMissingError
	if errors.As(err, &em) {
		return errConnLost
	}
	return errConnLost
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

type fatalErr struct{ error }

func isFatalSSH(err error) bool {
	var f fatalErr
	return errors.As(err, &f)
}

func humanSSHError(err error) error {
	m := err.Error()
	switch {
	case strings.Contains(m, "unable to authenticate"):
		return fatalErr{errors.New("неверный логин, пароль или ключ")}
	case strings.Contains(m, "ключ сервера изменился"):
		return fatalErr{err}
	case strings.Contains(m, "connection refused"):
		return errors.New("порт SSH закрыт (сервер ещё загружается или указан не тот порт)")
	case strings.Contains(m, "i/o timeout"), strings.Contains(m, "no route"):
		return errors.New("сервер не отвечает (проверьте IP-адрес)")
	}
	return err
}

// lockedBuf — буфер, в который безопасно пишут stdout и stderr одновременно.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Output2 — как Output, но stdout и stderr раздельно (stdout нужен для JSON).
func (r *Remote) Output2(cmd string, stdin []byte, timeout time.Duration) (string, string, int, error) {
	s, err := r.session()
	if err != nil {
		return "", "", -1, err
	}
	defer s.Close()
	so, se := &lockedBuf{}, &lockedBuf{}
	s.Stdout, s.Stderr = so, se
	if stdin != nil {
		s.Stdin = bytes.NewReader(stdin)
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(cmd) }()
	select {
	case err = <-done:
	case <-time.After(timeout):
		s.Close()
		r.dropClient()
		return so.String(), se.String(), -1, errTimeout
	}
	return so.String(), se.String(), exitCode(err), connErr(err)
}

// Alive — есть ли живое подключение.
func (r *Remote) Alive() bool {
	r.mu.Lock()
	c := r.client
	r.mu.Unlock()
	if c == nil {
		return false
	}
	done := make(chan error, 1)
	go func() { _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); done <- err }()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(6 * time.Second):
		c.Close()
		return false
	}
}
