//go:build !windows

// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"os/exec"
)

func openBrowser(url string) {
	if exec.Command("xdg-open", url).Start() != nil {
		fmt.Println("Откройте в браузере:", url)
	}
}

func openFolder(dir string) { _ = exec.Command("xdg-open", dir).Start() }
