# AmneziaWG Cascade

[Русская версия](README.md)

A Windows app that deploys a **two-server cascade VPN on AmneziaWG** in one run and then lets you manage it — no manual console work.

- **RU server** (entry, in Russia) — phones and computers connect to it. Russian websites are reached directly from it, with a Russian IP.
- **EU server** (exit, abroad) — all other traffic leaves through it.

One connection on the device, and both local and international services behave as expected.

![Users](docs/screenshots/01-users.png)

> The user interface is in Russian.

## Features

**Installation**

- Enter the IP and root password (or SSH key) of two servers; the app does the rest:
  1. **RU:** system update → AmneziaWG install;
  2. **EU:** system update → AmneziaWG install;
  3. **cascade:** RU → EU tunnel, split routing by the list of Russian networks, autostart, weekly list refresh, self-check.
- Servers reboot several times — the app waits and continues. If closed mid-install, it resumes on the next run.
- At the end you get QR codes and `.conf` files.

**Management**

- **Users:** online status, traffic, expiry; add, QR code and key, rename without changing the key, re-issue key, delete.
- **Statistics:** traffic per day and per user for 1/7/30/90 days, collected on the server every 5 minutes and kept across reboots.
- **Server:** health of the RU server, the tunnel to EU, routing, EU reachability; restart VPN.
- **Maintenance:** package updates, reboot and VPN removal for RU and EU separately, plus removal of the whole cascade (with a backup first).
- **Backups** of both servers stored on your PC, with restore.
- **Profiles:** several cascades in one app; an already running cascade can be attached.

## Requirements

- Windows 10/11 (the UI opens in a Microsoft Edge app window).
- Two VPS with **Ubuntu 24.04 / 26.04 or Debian 13**, root SSH access, a **public IPv4** on each (not behind NAT). Clean servers are recommended.
- Servers need access to GitHub (the AmneziaWG installer is downloaded from there) and to package repositories.

## Usage

1. Download the archive from [Releases](../../releases) and unzip it.
2. Run `AmneziaWG-Cascade.exe`; no installation needed.
3. The file is not code-signed, so Windows SmartScreen may warn: "More info" → "Run anyway". Checksums are in the release's `SHA256SUMS.txt`; binaries are built from source by GitHub Actions.

Data locations: profiles (passwords/keys encrypted with Windows DPAPI) — `%APPDATA%\AmneziaWG-Cascade\profiles.json`; log, client files and backups — `Documents\AmneziaWG-Cascade\`.

## How it works

The app connects over SSH and performs the steps from [CASCADE.md](https://github.com/bivlked/amneziawg-installer/blob/main/CASCADE.md) of [amneziawg-installer](https://github.com/bivlked/amneziawg-installer):

- `install_amneziawg.sh` is downloaded from the project's official releases and run with `--yes --disallow-ipv6 --route-all --subnet=…`;
- long operations (system update, installer) run on the server as `systemd-run` units; the app only follows their logs, so disconnects and reboots don't break them;
- for the cascade, [`assets/cascade-setup.sh`](assets/cascade-setup.sh) is placed on the RU server: it creates the `awg1` tunnel to EU and installs the unmodified `awg-routing.sh` from CASCADE.md;
- users are managed through the stock `manage_amneziawg.sh`.

The UI is a local web page: the app listens on `127.0.0.1` only and every request carries a random token.

## Security

- Profile passwords and keys are encrypted with Windows DPAPI (only your Windows account on this PC can decrypt them).
- The server's SSH host key is pinned on first connect; if it changes during installation, the app stops.
- Backups contain VPN users' private keys — treat them like passwords.
- The RU server sees user traffic before it enters the tunnel — it is a point of trust.

## Limitations

- IPv4 only; IPv6 is disabled on the servers.
- Traffic is split by destination IP, not by domain: a `.ru` site on a foreign CDN goes via EU (add such IPs to `EXTRA_RU_NETS` in `/root/awg/awg-routing.sh`).
- Restore targets the same server; moving to a server with another IP requires re-issuing client files.

## Building

Go 1.24+:

```bash
go mod tidy
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H=windowsgui -s -w -X main.version=1.3.0" -o AmneziaWG-Cascade.exe .
```

`rsrc_windows_amd64.syso` (icon and version info) is built from [`winres/`](winres) with `x86_64-w64-mingw32-windres -O coff -o ../rsrc_windows_amd64.syso app.rc`.

## Authorship

The application code was written by **Claude**, an AI model by [Anthropic](https://www.anthropic.com), at the request and under the direction of **AACH**, who defined the requirements, tested it on real servers and maintains the project.

The cascade design and routing script come from [amneziawg-installer](https://github.com/bivlked/amneziawg-installer) (© Ivan Bondarev, MIT). This project is not affiliated with AmneziaVPN or the author of amneziawg-installer.

## License

[MIT](LICENSE) © 2026 AACH. Third-party components: [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
