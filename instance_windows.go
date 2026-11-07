//go:build windows

// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

import (
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetExtendedTcpTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

// listenerPID — процесс, который слушает 127.0.0.1:port (0, если не найден).
func listenerPID(port int) uint32 {
	const afInet, tcpTableOwnerPIDListener = 2, 3
	var size uint32
	_, _, _ = procGetExtendedTcpTable.Call(0, uintptr(unsafe.Pointer(&size)), 0, afInet, tcpTableOwnerPIDListener, 0)
	if size == 0 {
		return 0
	}
	buf := make([]byte, size+1024)
	size = uint32(len(buf))
	r, _, _ := procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, afInet, tcpTableOwnerPIDListener, 0)
	if r != 0 {
		return 0
	}
	n := *(*uint32)(unsafe.Pointer(&buf[0]))
	type row struct{ State, LocalAddr, LocalPort, RemoteAddr, RemotePort, PID uint32 }
	for i := uint32(0); i < n; i++ {
		off := 4 + uintptr(i)*unsafe.Sizeof(row{})
		if off+unsafe.Sizeof(row{}) > uintptr(len(buf)) {
			break
		}
		rw := (*row)(unsafe.Pointer(&buf[off]))
		p := int(rw.LocalPort&0xff)<<8 | int(rw.LocalPort>>8&0xff)
		if p == port {
			return rw.PID
		}
	}
	return 0
}

// stopOldInstance завершает ранее запущенную копию программы. Сначала ищем процесс,
// который держит порт; если система его не сообщает — копии программы по имени файла.
// Чужие процессы не трогаем: имя exe должно начинаться с «AmneziaWG-Cascade».
func stopOldInstance(port int) bool {
	self := windows.GetCurrentProcessId()
	if pid := listenerPID(port); pid != 0 && pid != self {
		return killIfOurs(pid)
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	killed := false
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if pe.ProcessID != self && isOurExe(windows.UTF16ToString(pe.ExeFile[:])) && killIfOurs(pe.ProcessID) {
			killed = true
		}
	}
	return killed
}

func isOurExe(path string) bool {
	exe := strings.ToLower(filepath.Base(path))
	// Lite и полная версия работают рядом: трогаем только копии своей версии.
	return strings.HasPrefix(exe, "amneziawg-cascade") && strings.HasSuffix(exe, ".exe") &&
		strings.Contains(exe, "lite") == (exeMarker != "")
}

func killIfOurs(pid uint32) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	name := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(name))
	if windows.QueryFullProcessImageName(h, 0, &name[0], &n) != nil || !isOurExe(windows.UTF16ToString(name[:n])) {
		return false
	}
	if windows.TerminateProcess(h, 0) != nil {
		return false
	}
	_, _ = windows.WaitForSingleObject(h, 5000)
	return true
}
