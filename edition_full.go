// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

//go:build !lite

package main

import _ "embed"

// Полная версия: все функции, включая маршрутизацию по доменам и спискам.
// Lite собирается из того же кода с тегом lite (edition_lite.go).

//go:embed assets/index.html
var indexHTML string

const (
	edition   = "full"
	appPort   = 47821
	appID     = "AmneziaWG-Cascade"
	appTitle  = "AmneziaWG Cascade"
	exeMarker = "" // в имени exe полной версии нет «lite»
)
