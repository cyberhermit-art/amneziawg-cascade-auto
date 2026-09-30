//go:build windows

// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// openBrowser открывает интерфейс отдельным окном-приложением Edge (есть в любой
// Windows 10/11), иначе Chrome, иначе браузером по умолчанию.
func openBrowser(url string) {
	var cands []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
		base := os.Getenv(env)
		if base == "" {
			continue
		}
		cands = append(cands,
			filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"))
	}
	for _, p := range cands {
		if _, err := os.Stat(p); err == nil {
			cmd := exec.Command(p, "--app="+url, "--window-size=1100,860", "--new-window")
			cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
			if cmd.Start() == nil {
				return
			}
		}
	}
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Start()
}

func openFolder(dir string) {
	_ = exec.Command("explorer", dir).Start()
}
