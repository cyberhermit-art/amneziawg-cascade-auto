// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Event — сообщение для интерфейса.
type Event struct {
	T      string       `json:"t"`             // log | stage | finish
	Src    string       `json:"src,omitempty"` // RU | EU | ""
	Msg    string       `json:"msg,omitempty"`
	ID     string       `json:"id,omitempty"`    // для stage
	State  string       `json:"state,omitempty"` // run | ok | err | skip
	OK     bool         `json:"ok,omitempty"`
	Result *FinalResult `json:"result,omitempty"`
	Time   string       `json:"time,omitempty"`
}

type ClientFile struct {
	Name   string `json:"name"`
	Conf   string `json:"conf"`
	VpnURI string `json:"vpnuri,omitempty"`
	PNG    string `json:"png,omitempty"` // base64
}

type FinalResult struct {
	Error   string       `json:"error,omitempty"`
	Clients []ClientFile `json:"clients,omitempty"`
	Folder  string       `json:"folder,omitempty"`
	EUIP    string       `json:"euIp,omitempty"`
	RUIP    string       `json:"ruIp,omitempty"`
	Profile string       `json:"profile,omitempty"`
}

type Hub struct {
	mu      sync.Mutex
	events  [][]byte
	subs    map[chan []byte]struct{}
	running bool
	lastSub time.Time
	logSink func(string)
}

func NewHub() *Hub {
	return &Hub{subs: map[chan []byte]struct{}{}, lastSub: time.Now()}
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func clean(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	if i := strings.LastIndex(s, "\r"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimRight(s, " \t")
}

func (h *Hub) emit(e Event) {
	e.Time = time.Now().Format("15:04:05")
	b, _ := json.Marshal(e)
	h.mu.Lock()
	h.events = append(h.events, b)
	// не даём истории расти бесконечно — держим последние 20000 событий
	if len(h.events) > 20000 {
		h.events = append([][]byte(nil), h.events[len(h.events)-15000:]...)
	}
	for c := range h.subs {
		select {
		case c <- b:
		default:
		}
	}
	sink := h.logSink
	h.mu.Unlock()
	if sink != nil && e.T == "log" {
		prefix := ""
		if e.Src != "" {
			prefix = "[" + e.Src + "] "
		}
		sink(e.Time + " " + prefix + e.Msg)
	}
}

func (h *Hub) Log(src, msg string) {
	for _, line := range strings.Split(msg, "\n") {
		line = clean(line)
		if strings.TrimSpace(line) == "" {
			continue
		}
		h.emit(Event{T: "log", Src: src, Msg: line})
	}
}

func (h *Hub) Stage(id, state, msg string) {
	h.emit(Event{T: "stage", ID: id, State: state, Msg: msg})
}

func (h *Hub) Reset() {
	h.mu.Lock()
	h.events = nil
	h.mu.Unlock()
	h.emit(Event{T: "reset"})
}

func (h *Hub) Subscribe() (chan []byte, [][]byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := make(chan []byte, 4096)
	h.subs[c] = struct{}{}
	h.lastSub = time.Now()
	backlog := append([][]byte(nil), h.events...)
	return c, backlog
}

func (h *Hub) Unsubscribe(c chan []byte) {
	h.mu.Lock()
	delete(h.subs, c)
	h.lastSub = time.Now()
	h.mu.Unlock()
}

// Idle — никто не смотрит и ничего не выполняется.
func (h *Hub) Idle(d time.Duration) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) == 0 && !h.running && time.Since(h.lastSub) > d
}

func (h *Hub) SetRunning(v bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if v && h.running {
		return false
	}
	h.running = v
	return true
}
