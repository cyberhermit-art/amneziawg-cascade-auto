// Copyright (c) 2026 AACH
// SPDX-License-Identifier: MIT

package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Profile — один каскад (пара серверов RU + EU) с данными для входа.
type Profile struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	RU       ServerCfg `json:"ru"`
	EU       ServerCfg `json:"eu"`
	RUSubnet string    `json:"ruSubnet,omitempty"`
	EUSubnet string    `json:"euSubnet,omitempty"`
	Created  string    `json:"created,omitempty"`
}

// storedServer — как сервер лежит на диске: секреты зашифрованы.
type storedServer struct {
	Host   string `json:"host"`
	Port   int    `json:"port"`
	User   string `json:"user"`
	Secret string `json:"secret,omitempty"` // base64(protect(json{password,key,keyPass}))
	HasKey bool   `json:"hasKey,omitempty"`
}

type storedProfile struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	RU       storedServer `json:"ru"`
	EU       storedServer `json:"eu"`
	RUSubnet string       `json:"ruSubnet,omitempty"`
	EUSubnet string       `json:"euSubnet,omitempty"`
	Created  string       `json:"created,omitempty"`
}

type storeFile struct {
	Version  int             `json:"version"`
	Scheme   string          `json:"scheme"`
	Last     string          `json:"last,omitempty"`
	Profiles []storedProfile `json:"profiles"`
}

type secrets struct {
	Password string `json:"p,omitempty"`
	Key      string `json:"k,omitempty"`
	KeyPass  string `json:"kp,omitempty"`
}

var profMu sync.Mutex

func appDataDir() string {
	if d := os.Getenv("AWGC_DATA"); d != "" { // для тестов
		return d
	}
	base, err := os.UserConfigDir() // Windows: %APPDATA%
	if err != nil {
		home, _ := os.UserHomeDir()
		base = home
	}
	return filepath.Join(base, "AmneziaWG-Cascade")
}

func profilesPath() string { return filepath.Join(appDataDir(), "profiles.json") }

// ProfileDir — папка профиля в «Документах»: резервные копии и файлы клиентов.
func profileDir(p *Profile) string {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, "Documents")
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		base = home
	}
	return filepath.Join(base, "AmneziaWG-Cascade", safeName(p.Name+"-"+p.RU.Host))
}

func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"/\|?*`, r) || r < 32 {
			return '_'
		}
		return r
	}, s)
	s = strings.Trim(s, " .")
	if s == "" {
		s = "cascade"
	}
	return s
}

func encodeServer(s ServerCfg) (storedServer, error) {
	sec, _ := json.Marshal(secrets{Password: s.Password, Key: s.Key, KeyPass: s.KeyPass})
	enc, err := protect(sec)
	if err != nil {
		return storedServer{}, err
	}
	return storedServer{Host: s.Host, Port: s.Port, User: s.User, Secret: base64.StdEncoding.EncodeToString(enc), HasKey: s.Key != ""}, nil
}

func decodeServer(st storedServer, role string) ServerCfg {
	s := ServerCfg{Role: role, Host: st.Host, Port: st.Port, User: st.User}
	if st.Secret != "" {
		if enc, err := base64.StdEncoding.DecodeString(st.Secret); err == nil {
			if plain, err := unprotect(enc); err == nil {
				var sec secrets
				if json.Unmarshal(plain, &sec) == nil {
					s.Password, s.Key, s.KeyPass = sec.Password, sec.Key, sec.KeyPass
				}
			}
		}
	}
	return s
}

func loadStore() (*storeFile, error) {
	b, err := os.ReadFile(profilesPath())
	if errors.Is(err, os.ErrNotExist) {
		return &storeFile{Version: 1, Scheme: secretScheme}, nil
	}
	if err != nil {
		return nil, err
	}
	var sf storeFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return nil, errors.New("файл профилей повреждён: " + err.Error())
	}
	return &sf, nil
}

func saveStore(sf *storeFile) error {
	if err := os.MkdirAll(appDataDir(), 0o700); err != nil {
		return err
	}
	sf.Version, sf.Scheme = 1, secretScheme
	b, _ := json.MarshalIndent(sf, "", "  ")
	tmp := profilesPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, profilesPath())
}

func listProfiles() ([]Profile, string, error) {
	profMu.Lock()
	defer profMu.Unlock()
	sf, err := loadStore()
	if err != nil {
		return nil, "", err
	}
	var out []Profile
	for _, sp := range sf.Profiles {
		out = append(out, fromStored(sp))
	}
	sort.SliceStable(out, func(a, b int) bool { return strings.ToLower(out[a].Name) < strings.ToLower(out[b].Name) })
	return out, sf.Last, nil
}

func fromStored(sp storedProfile) Profile {
	return Profile{ID: sp.ID, Name: sp.Name, RU: decodeServer(sp.RU, "RU"), EU: decodeServer(sp.EU, "EU"),
		RUSubnet: sp.RUSubnet, EUSubnet: sp.EUSubnet, Created: sp.Created}
}

func getProfile(id string) (*Profile, error) {
	ps, _, err := listProfiles()
	if err != nil {
		return nil, err
	}
	for i := range ps {
		if ps[i].ID == id {
			return &ps[i], nil
		}
	}
	return nil, errors.New("профиль не найден")
}

// saveProfile создаёт или обновляет профиль. Пустые пароль/ключ при
// обновлении означают «оставить прежние».
func saveProfile(p Profile) (*Profile, error) {
	profMu.Lock()
	defer profMu.Unlock()
	sf, err := loadStore()
	if err != nil {
		return nil, err
	}
	p.Name = strings.TrimSpace(p.Name)
	for _, sv := range []*ServerCfg{&p.RU, &p.EU} {
		if sv.User == "" {
			sv.User = "root"
		}
		if sv.Port == 0 {
			sv.Port = 22
		}
	}
	if p.Name == "" {
		p.Name = "Каскад " + p.RU.Host
	}
	idx := -1
	for i, sp := range sf.Profiles {
		if (p.ID != "" && sp.ID == p.ID) || (p.ID == "" && sp.RU.Host == p.RU.Host && sp.RU.Port == p.RU.Port) {
			idx = i
			break
		}
	}
	if idx >= 0 {
		old := fromStored(sf.Profiles[idx])
		p.ID = old.ID
		if p.Created == "" {
			p.Created = old.Created
		}
		keepSecrets(&p.RU, old.RU)
		keepSecrets(&p.EU, old.EU)
	} else {
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		p.ID = hex.EncodeToString(b)
		p.Created = time.Now().Format("2006-01-02 15:04")
	}
	ru, err := encodeServer(p.RU)
	if err != nil {
		return nil, err
	}
	eu, err := encodeServer(p.EU)
	if err != nil {
		return nil, err
	}
	sp := storedProfile{ID: p.ID, Name: p.Name, RU: ru, EU: eu, RUSubnet: p.RUSubnet, EUSubnet: p.EUSubnet, Created: p.Created}
	if idx >= 0 {
		sf.Profiles[idx] = sp
	} else {
		sf.Profiles = append(sf.Profiles, sp)
	}
	sf.Last = p.ID
	if err := saveStore(sf); err != nil {
		return nil, err
	}
	return &p, nil
}

func keepSecrets(n *ServerCfg, old ServerCfg) {
	if n.Password == "" && n.Key == "" {
		n.Password, n.Key, n.KeyPass = old.Password, old.Key, old.KeyPass
	}
}

func deleteProfile(id string) error {
	profMu.Lock()
	defer profMu.Unlock()
	sf, err := loadStore()
	if err != nil {
		return err
	}
	out := sf.Profiles[:0]
	for _, sp := range sf.Profiles {
		if sp.ID != id {
			out = append(out, sp)
		}
	}
	sf.Profiles = out
	if sf.Last == id {
		sf.Last = ""
	}
	return saveStore(sf)
}

func setLastProfile(id string) {
	profMu.Lock()
	defer profMu.Unlock()
	if sf, err := loadStore(); err == nil && sf.Last != id {
		sf.Last = id
		_ = saveStore(sf)
	}
}

// publicProfile — профиль для интерфейса, без секретов.
type publicProfile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RUHost   string `json:"ruHost"`
	RUPort   int    `json:"ruPort"`
	RUUser   string `json:"ruUser"`
	RUHasKey bool   `json:"ruHasKey"`
	EUHost   string `json:"euHost"`
	EUPort   int    `json:"euPort"`
	EUUser   string `json:"euUser"`
	EUHasKey bool   `json:"euHasKey"`
	Created  string `json:"created"`
}

func toPublic(p Profile) publicProfile {
	return publicProfile{ID: p.ID, Name: p.Name, RUHost: p.RU.Host, RUPort: p.RU.Port, RUUser: p.RU.User, RUHasKey: p.RU.Key != "",
		EUHost: p.EU.Host, EUPort: p.EU.Port, EUUser: p.EU.User, EUHasKey: p.EU.Key != "", Created: p.Created}
}
