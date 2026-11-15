//go:build !windows

// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

func stopOldInstance(port int) bool { return false }

func listenerIsMine(port int) bool { return true }

func showError(title, text string) { println(title + ": " + text) }
