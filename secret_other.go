//go:build !windows

// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

// Вне Windows (только для тестов) шифрование не применяется.
const secretScheme = "plain"

func protect(plain []byte) ([]byte, error) { return append([]byte(nil), plain...), nil }
func unprotect(enc []byte) ([]byte, error) { return append([]byte(nil), enc...), nil }
