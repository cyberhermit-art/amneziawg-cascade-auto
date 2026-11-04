# Third-party notices / Сторонние компоненты

AmneziaWG Cascade includes or depends on the following third-party software.
Программа включает или использует следующие сторонние компоненты.

---

## 1. amneziawg-installer — cascade routing script

- Project: https://github.com/bivlked/amneziawg-installer
- Used: the routing script `awg-routing.sh` from `CASCADE.md` is embedded unmodified
  in `assets/cascade-setup.sh` (between the `ROUTING_EOF` markers).
  The installer `install_amneziawg.sh` itself is **not** distributed: the program
  downloads it from the project's official GitHub releases at install time.
- License: MIT

```
MIT License

Copyright (c) 2025-2026 Ivan Bondarev

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

---

## 1a. Russian IPv4 network list (downloaded at runtime, not distributed)

- Source: https://github.com/antonme/GeoIP — `russia.ipv4.cidr.txt`
- Used: the routing script on the RU server downloads this list and merges it with
  the ipdeny.com list. The list is **not** included in this repository or in the program.
- The list is derived from MaxMind GeoLite2 data; use of the data is subject to the
  GeoLite2 End User License Agreement: https://www.maxmind.com/en/geolite2/eula
  This product includes GeoLite2 data created by MaxMind, available from https://www.maxmind.com.

## 1b. DB-IP Lite country data (downloaded at runtime, not distributed)

- Source: https://github.com/v2fly/geoip — `text/ru.txt` (release branch), built from DB-IP Lite data.
- Used: merged into the list of Russian networks by the routing script on the RU server.
- License of the data: Creative Commons Attribution 4.0 International — IP Geolocation by DB-IP (https://db-ip.com).

## 1c. Russian service domain list (downloaded at runtime, not distributed)

- Source: https://github.com/lib4u/amnezia-tunneling-ru — `amnezia.json`, generated from
  https://github.com/v2fly/domain-list-community (MIT License).
- Used: only if «Домены РФ» is enabled; the RU server builds a dnsmasq configuration from it.
- dnsmasq itself (GPL-2.0/GPL-3.0, https://thekelleys.org.uk/dnsmasq/) is installed on the
  server from the operating system's package repository and is not distributed with this product.

---

## 2. Go supplementary libraries: golang.org/x/crypto, golang.org/x/sys

- Projects: https://go.googlesource.com/crypto, https://go.googlesource.com/sys
- Used: SSH client (`golang.org/x/crypto/ssh`), Windows DPAPI (`golang.org/x/sys/windows`);
  compiled into the Windows executable.
- License: BSD-3-Clause

```
Copyright 2009 The Go Authors.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google LLC nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```

---

## 3. Go standard library and runtime

- Project: https://go.dev — compiled into the executable.
- License: BSD-3-Clause (same text as above, Copyright 2009 The Go Authors).
