// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestValidate(t *testing.T) {
	srv := func(h string) ServerCfg { return ServerCfg{Host: h, Password: "x"} }
	cases := []struct {
		name string
		in   JobInput
		err  string // часть текста ошибки; "" — без ошибки
	}{
		{"два сервера по умолчанию", JobInput{RU: srv("1.1.1.1"), EU: srv("2.2.2.2")}, ""},
		{"нет адреса EU", JobInput{RU: srv("1.1.1.1"), EU: ServerCfg{Password: "x"}}, "не указан IP-адрес сервера EU"},
		{"нет пароля и ключа", JobInput{RU: srv("1.1.1.1"), EU: ServerCfg{Host: "2.2.2.2"}}, "укажите пароль или SSH-ключ"},
		{"один и тот же сервер", JobInput{RU: srv("1.1.1.1"), EU: srv(" 1.1.1.1 ")}, "одинаковый адрес"},
		{"неверный порт", JobInput{RU: srv("1.1.1.1"), EU: ServerCfg{Host: "2.2.2.2", Port: 70000, Password: "x"}}, "неверный порт SSH"},
		{"неизвестный протокол", JobInput{RU: srv("1.1.1.1"), EU: srv("2.2.2.2"), Protocol: "9"}, "неизвестная версия протокола"},
		{"подсети пересекаются", JobInput{RU: srv("1.1.1.1"), EU: srv("2.2.2.2"), RUSubnet: "10.0.0.1/16", EUSubnet: "10.0.5.1/24"}, "пересекаются"},
		{"подсеть с мусором", JobInput{RU: srv("1.1.1.1"), EU: srv("2.2.2.2"), RUSubnet: "10.0.0.1/24; reboot"}, "подсеть указана неверно"},
		{"выход совпадает с RU", JobInput{RU: srv("1.1.1.1"), EU: srv("2.2.2.2"), EX: srv("1.1.1.1")}, "тот же адрес"},
		{"подсеть выхода пересекается", JobInput{RU: srv("1.1.1.1"), EU: srv("2.2.2.2"), EX: srv("3.3.3.3"), EXSubnet: "172.16.17.5/24"}, "подсеть сервера-выхода пересекается"},
		{"три сервера", JobInput{RU: srv("1.1.1.1"), EU: srv("2.2.2.2"), EX: srv("3.3.3.3")}, ""},
		{"плохое имя клиента", JobInput{RU: srv("1.1.1.1"), EU: srv("2.2.2.2"), ExtraClients: "anna, ivan.petrov"}, "имя клиента"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.in
			err := in.validate()
			if c.err == "" {
				if err != nil {
					t.Fatalf("неожиданная ошибка: %v", err)
				}
				if in.Protocol != "3.1" || in.RUSubnet == "" || in.EUSubnet == "" || in.RU.Port != 22 {
					t.Fatalf("не заполнены значения по умолчанию: %+v", in)
				}
				if in.threeHop() != (c.in.EX.Host != "") || (in.threeHop() && in.EXSubnet == "") {
					t.Fatalf("неверная настройка третьего сервера: %+v", in)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("ждали ошибку с %q, получили %v", c.err, err)
			}
		})
	}
}

func TestVersionLess(t *testing.T) {
	for _, c := range []struct {
		a, b string
		less bool
	}{
		{"1.9.0", "1.10.0", true}, {"1.17.0", "1.17.1", true}, {"1.17", "1.17.0", false},
		{"2.0.0", "1.99.9", false}, {"1.17.1", "1.17.1", false},
	} {
		if got := versionLess(c.a, c.b); got != c.less {
			t.Errorf("versionLess(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

// Ключ сервера запоминается между «запусками» (новыми Remote) и подмена замечается.
func TestHostKeyRemembered(t *testing.T) {
	t.Setenv("AWGC_DATA", t.TempDir())
	key := func() ssh.PublicKey {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		k, _ := ssh.NewPublicKey(pub)
		return k
	}
	k1, k2 := key(), key()
	check := func(k ssh.PublicKey) error {
		r := NewRemote(ServerCfg{Host: "203.0.113.5", Port: 22, Password: "x"}, func(string) {})
		cfg, err := r.sshConfig()
		if err != nil {
			t.Fatal(err)
		}
		return cfg.HostKeyCallback("", nil, k)
	}
	if err := check(k1); err != nil {
		t.Fatalf("первое подключение: %v", err)
	}
	if err := check(k1); err != nil {
		t.Fatalf("тот же ключ после «перезапуска»: %v", err)
	}
	err := check(k2)
	if err == nil || !isFatalSSH(humanSSHError(err)) || !strings.Contains(err.Error(), "[[HOSTKEY 203.0.113.5:22]]") {
		t.Fatalf("подмена ключа не замечена: %v", err)
	}
	if err := setHostKey("203.0.113.5:22", ""); err != nil { // «Доверять новому ключу»
		t.Fatal(err)
	}
	if err := check(k2); err != nil {
		t.Fatalf("новый ключ после подтверждения: %v", err)
	}
	if clean("ошибка [[HOSTKEY 1.2.3.4:22]]") != "ошибка" {
		t.Fatal("метка HOSTKEY попадает в журнал")
	}
}

// Пока запрос к API выполняется, программа не должна закрываться сама.
func TestIdleWaitsForRequests(t *testing.T) {
	token = "t"
	h := &Hub{subs: map[chan []byte]struct{}{}, lastSub: time.Now().Add(-time.Hour)}
	old := hub
	hub = h
	defer func() { hub = old }()
	inside := make(chan bool, 1)
	srv := httptest.NewServer(api(func(w http.ResponseWriter, r *http.Request) { inside <- hub.Idle(time.Second) }))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	req.Header.Set("X-Token", "t")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if <-inside {
		t.Fatal("во время запроса программа считает себя простаивающей")
	}
	if !hub.Idle(time.Second) {
		t.Fatal("после запроса программа должна считать себя простаивающей")
	}
}

func TestPapi(t *testing.T) {
	t.Setenv("AWGC_DATA", t.TempDir())
	token = "t"
	p, err := saveProfile(Profile{Name: "Тест", RU: ServerCfg{Host: "1.1.1.1", Password: "x"}, EU: ServerCfg{Host: "2.2.2.2", Password: "y"}})
	if err != nil {
		t.Fatal(err)
	}
	h := papi(func(m *mctx, r toolReq) (any, error) {
		return map[string]any{"name": m.p.Name, "port": r.Port}, nil
	})
	do := func(body string) string {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("X-Token", "t")
		h(w, req)
		return strings.TrimSpace(w.Body.String())
	}
	if got := do(`{"id":"` + p.ID + `","port":443}`); got != `{"name":"Тест","port":443}` {
		t.Fatalf("ответ: %s", got)
	}
	if got := do(`{"id":"nope"}`); !strings.Contains(got, "профиль не найден") {
		t.Fatalf("чужой профиль: %s", got)
	}
	if got := do(`{`); !strings.Contains(got, "неверный запрос") {
		t.Fatalf("испорченный запрос: %s", got)
	}
}

// После смены порта на EU/транзите сервер RU обязан перезапустить awg-routing,
// иначе маршрут в таблицу 100 пропадает и защита блокирует зарубежный трафик.
func TestPortPeer(t *testing.T) {
	if peer, _, _, restart := portPeer("EU"); peer != "RU" || !strings.Contains(restart, "restart awg-routing") {
		t.Fatalf("EU: peer=%s restart=%q", peer, restart)
	}
	if peer, _, extra, _ := portPeer("EX"); peer != "EU" || extra != "/root/mid_host.conf" {
		t.Fatalf("EX: peer=%s extra=%s", peer, extra)
	}
	if peer, _, _, _ := portPeer("RU"); peer != "" {
		t.Fatalf("RU: peer=%s", peer)
	}
}
