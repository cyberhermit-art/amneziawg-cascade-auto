// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

import (
	_ "embed"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
)

//go:embed assets/cascade-setup.sh
var cascadeScript []byte

//go:embed assets/vps-update.sh
var updateScript []byte

const (
	installerURL  = "https://github.com/bivlked/amneziawg-installer/releases/latest/download/install_amneziawg.sh"
	installerPath = "/root/install_amneziawg.sh"
	installLog    = "/root/awg-cascade-install.log"
	updatePath    = "/root/awgc-vps-update.sh"
	updateLog     = "/root/awgc-update.log"
	updateDone    = "/root/.awgc-update-done"
)

type JobInput struct {
	RU           ServerCfg `json:"ru"`
	EU           ServerCfg `json:"eu"`
	RUSubnet     string    `json:"ruSubnet"`
	EUSubnet     string    `json:"euSubnet"`
	ExtraClients string    `json:"extraClients"`
	SkipUpdate   bool      `json:"skipUpdate"`
	ProfileName  string    `json:"profileName"`
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

func (in *JobInput) validate() error {
	in.RU.Role, in.EU.Role = "RU", "EU"
	for _, s := range []*ServerCfg{&in.RU, &in.EU} {
		s.Host = strings.TrimSpace(s.Host)
		if s.Host == "" {
			return fmt.Errorf("не указан IP-адрес сервера %s", s.Role)
		}
		if s.Port == 0 {
			s.Port = 22
		}
		if s.Port < 1 || s.Port > 65535 {
			return fmt.Errorf("неверный порт SSH у сервера %s", s.Role)
		}
		if s.Password == "" && strings.TrimSpace(s.Key) == "" {
			return fmt.Errorf("для сервера %s укажите пароль или SSH-ключ", s.Role)
		}
	}
	if in.RU.addr() == in.EU.addr() {
		return errors.New("у серверов RU и EU одинаковый адрес — нужны два разных сервера")
	}
	if in.RUSubnet == "" {
		in.RUSubnet = "172.16.17.1/24"
	}
	if in.EUSubnet == "" {
		in.EUSubnet = "172.16.61.1/24"
	}
	_, n1, err1 := net.ParseCIDR(in.RUSubnet)
	_, n2, err2 := net.ParseCIDR(in.EUSubnet)
	if err1 != nil || err2 != nil || n1.IP.To4() == nil || n2.IP.To4() == nil {
		return errors.New("подсеть указана неверно (пример: 172.16.17.1/24)")
	}
	if n1.Contains(n2.IP) || n2.Contains(n1.IP) {
		return errors.New("подсети серверов пересекаются — они должны быть разными")
	}
	for _, n := range extraNames(in.ExtraClients) {
		if !nameRe.MatchString(n) {
			return fmt.Errorf("имя клиента %q: допустимы латинские буквы, цифры, _ и - (до 32 символов)", n)
		}
	}
	return nil
}

func extraNames(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
