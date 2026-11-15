// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Логика переключения выходов (assets/awgc-exits.sh run) на заглушках ip/curl:
// текущий выход переключается только после двух неудачных проверок подряд,
// а на основной трафик возвращается после трёх удачных.
func TestExitsFailover(t *testing.T) {
	for _, b := range []string{"bash", "flock", "awk", "sed"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip("нет " + b)
		}
	}
	d := t.TempDir()
	A, bin := filepath.Join(d, "awg"), filepath.Join(d, "bin")
	for _, p := range []string{A, bin} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := strings.Replace(strings.Replace(string(exitsScript),
		"\nA=/root/awg\n", "\nA="+A+"\n", 1),
		"\nC=/etc/amnezia/amneziawg\n", "\nC="+d+"\n", 1)
	if !strings.Contains(script, "A="+A) {
		t.Fatal("в скрипте не найдена строка A=/root/awg")
	}
	sp := filepath.Join(d, "x.sh")
	mocks := map[string]string{
		// ip: «link show IF» — интерфейс есть всегда; остальное записываем в журнал
		"ip": `echo "ip $*" >> "$T/log"; exit 0`,
		// curl --interface IF: отвечает 204, только если существует файл $T/ok-IF
		"curl":      `for a in "$@"; do [ "$p" = --interface ] && i=$a; p=$a; done; [ -f "$T/ok-$i" ] && { echo -n 204; exit 0; }; exit 7`,
		"systemctl": `exit 0`, "awg": `exit 0`, "iptables": `exit 0`,
	}
	for n, body := range mocks {
		if err := os.WriteFile(filepath.Join(bin, n), []byte("#!/bin/bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(sp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(A, "awgc-exits.conf"), []byte("awg1 основной\nawg2 резерв-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	up := func(i string, ok bool) {
		f := filepath.Join(d, "ok-"+i)
		if ok {
			_ = os.WriteFile(f, nil, 0o644)
		} else {
			_ = os.Remove(f)
		}
	}
	run := func() string {
		cmd := exec.Command("bash", sp, "run")
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "T="+d)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("run: %v\n%s", err, out)
		}
		b, _ := os.ReadFile(filepath.Join(A, "awgc-exits", "current"))
		c := strings.TrimSpace(string(b))
		if c == "" {
			c = "awg1"
		}
		return c
	}
	up("awg1", true)
	up("awg2", true)
	steps := []struct {
		awg1 bool
		want string
	}{
		{true, "awg1"},
		{false, "awg1"}, // одна неудача — ещё не переключаемся
		{false, "awg2"}, // вторая подряд — на резерв
		{true, "awg2"},  // основной ожил: 1 удачная проверка
		{true, "awg2"},  // 2
		{true, "awg1"},  // 3 — возвращаемся
	}
	for i, s := range steps {
		up("awg1", s.awg1)
		if got := run(); got != s.want {
			t.Fatalf("шаг %d: текущий выход %s, ждали %s", i+1, got, s.want)
		}
	}
	// не работает ни один выход — остаёмся на текущем (трафик блокируется, а не утекает)
	up("awg1", false)
	up("awg2", false)
	for i := 0; i < 3; i++ {
		if got := run(); got != "awg1" {
			t.Fatalf("все выходы мертвы: переключились на %s", got)
		}
	}
}
