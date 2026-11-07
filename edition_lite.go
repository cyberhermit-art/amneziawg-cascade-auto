// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

//go:build lite

package main

import _ "embed"

// Lite: каскад из двух или трёх серверов, пользователи, серверы и резервные
// копии — без «Доменов РФ», «Мимо VPN» и «Через EU». Интерфейс на русском и
// английском. Свой порт, чтобы Lite и полная версия могли работать рядом;
// профили общие.

//go:embed assets/lite.html
var indexHTML string

const (
	edition   = "lite"
	appPort   = 47822
	appID     = "AmneziaWG-Cascade-Lite"
	appTitle  = "AmneziaWG Cascade Lite"
	exeMarker = "lite"
)
