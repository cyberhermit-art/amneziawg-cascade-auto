#!/usr/bin/env python3
# Copyright (c) 2026 AACH
# SPDX-License-Identifier: MIT
#
# awgc-bot.py — Telegram-бот на сервере RU каскада AmneziaWG Cascade:
# список пользователей, добавление, QR-код и файл подключения, удаление,
# состояние каскада. Отвечает только в чате владельца (TG_CHAT из
# /root/awg/awgc-agent.conf); остальные сообщения молча игнорирует.
#
# Запросы к Telegram идут через curl: сначала через туннель awg1 (из России
# api.telegram.org может быть недоступен), при неудаче — напрямую.
# Только стандартная библиотека Python 3.

import json
import os
import re
import shlex
import subprocess
import sys
import tempfile
import time

A = "/root/awg"
CONF = A + "/awgc-agent.conf"
MANAGE = A + "/manage_amneziawg.sh"
AWG0 = "/etc/amnezia/amneziawg/awg0.conf"
EXCL_ALLOWED = A + "/awgc-exclude.allowed"
LOG = A + "/awgc-agent/bot.log"
NAME_RE = re.compile(r"^[A-Za-z0-9_-]{1,32}$")
RESERVED = {"ru_host", "mid_host"}

EXPIRY_BUTTONS = [("Бессрочно", ""), ("7 дней", "7d"), ("30 дней", "30d"), ("90 дней", "90d"), ("1 год", "365d")]

HELP = (
    "Команды:\n"
    "/users — пользователи и их состояние\n"
    "/add — добавить пользователя (можно сразу: /add имя 30d)\n"
    "/get имя — QR-код и файл подключения\n"
    "/del имя — удалить пользователя\n"
    "/status — состояние каскада\n"
    "/proxy — ссылка на MTProto-прокси для Telegram (если включён)\n\n"
    "Срок: 12h, 30d, 2w или просто число дней. Без срока — бессрочно."
)


def log(msg):
    try:
        os.makedirs(os.path.dirname(LOG), exist_ok=True)
        if os.path.exists(LOG) and os.path.getsize(LOG) > 1_000_000:
            os.replace(LOG, LOG + ".1")
        with open(LOG, "a", encoding="utf-8") as f:
            f.write(time.strftime("%Y-%m-%d %H:%M:%S ") + msg + "\n")
    except OSError:
        pass


def read_conf():
    kv = {}
    try:
        for line in open(CONF, encoding="utf-8"):
            line = line.strip()
            if "=" in line and not line.startswith("#"):
                k, v = line.split("=", 1)
                try:
                    v = shlex.split(v)[0] if v else ""
                except ValueError:
                    pass
                kv[k] = v
    except OSError:
        pass
    return kv


class TG:
    def __init__(self, token):
        self.base = "https://api.telegram.org/bot" + token + "/"
        self.iface = "awg1"  # предпочтительный путь; меняется, если не работает

    def _routes(self):
        if not os.path.exists("/sys/class/net/awg1"):
            return [""]
        return [self.iface, "" if self.iface == "awg1" else "awg1"]

    def call(self, method, params=None, files=None, timeout=40):
        args = []
        for k, v in (params or {}).items():
            if isinstance(v, (dict, list)):
                v = json.dumps(v, ensure_ascii=False)
            args += ["--form-string", "%s=%s" % (k, v)]
        for k, path in (files or {}).items():
            args += ["-F", "%s=@%s" % (k, path)]
        last = None
        for r in self._routes():
            # адрес с токеном передаём через stdin (-K -): аргументы процесса видны всем в ps
            cmd = ["curl", "-s", "-m", str(timeout)] + (["--interface", r] if r else []) + args + ["-K", "-"]
            cfg = ('url = "%s%s"\n' % (self.base, method)).encode()
            try:
                out = subprocess.run(cmd, input=cfg, capture_output=True, timeout=timeout + 10).stdout
                res = json.loads(out.decode("utf-8", "replace") or "{}")
            except (subprocess.SubprocessError, ValueError, OSError) as e:
                last = str(e)
                continue
            if res.get("ok"):
                self.iface = r
                return res.get("result")
            last = res.get("description", "нет ответа")
            if res.get("error_code") in (400, 401, 403, 404, 409):
                break  # ответ от Telegram получен — другим путём не поможет
        raise RuntimeError(last or "нет связи с Telegram")

    def send(self, chat, text, kb=None):
        p = {"chat_id": chat, "text": text, "parse_mode": "HTML", "disable_web_page_preview": "true"}
        if kb:
            p["reply_markup"] = {"inline_keyboard": kb}
        return self.call("sendMessage", p)


def esc(s):
    return str(s).replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def sh(cmd, timeout=240):
    p = subprocess.run(["bash", "-c", cmd], capture_output=True, timeout=timeout)
    return p.returncode, p.stdout.decode("utf-8", "replace"), p.stderr.decode("utf-8", "replace")


def manage_json(what):
    rc, out, _ = sh("bash %s --json --no-color %s 2>/dev/null" % (MANAGE, what), 60)
    for line in out.splitlines():
        line = line.strip()
        if line.startswith("[") or line.startswith("{"):
            try:
                return json.loads(line)
            except ValueError:
                continue
    return []


def users():
    lst = {u.get("name"): u for u in manage_json("list") if isinstance(u, dict)}
    for s in manage_json("stats"):
        if isinstance(s, dict) and s.get("name") in lst:
            lst[s["name"]].update({"rx": s.get("rx", 0), "tx": s.get("tx", 0), "hs": s.get("last_handshake", 0), "sc": s.get("status_code", "")})
    return [u for n, u in sorted(lst.items()) if n and n not in RESERVED]


def exists(name):
    try:
        return ("#_Name = " + name) in open(AWG0, encoding="utf-8").read().splitlines()
    except OSError:
        return False


def fmt_bytes(n):
    n = float(n or 0)
    for u in ("Б", "КБ", "МБ", "ГБ", "ТБ"):
        if n < 1024 or u == "ТБ":
            return ("%d %s" % (n, u)) if u == "Б" else ("%.1f %s" % (n, u))
        n /= 1024


def ago(sec):
    if sec < 60:
        return "только что"
    if sec < 3600:
        return "%d мин назад" % (sec // 60)
    if sec < 86400:
        return "%d ч назад" % (sec // 3600)
    return "%d дн назад" % (sec // 86400)


def parse_expiry(s):
    s = (s or "").strip().lower()
    if s in ("", "0", "-", "бессрочно", "never"):
        return ""
    if re.fullmatch(r"\d{1,4}", s):
        return s + "d"
    if re.fullmatch(r"\d{1,4}[hdw]", s):
        return s
    m = re.fullmatch(r"(\d{1,2})y", s)
    if m:
        return "%dd" % (int(m.group(1)) * 365)
    return None


def human_expiry(e):
    n, u = int(e[:-1]), e[-1]
    return "%d %s" % (n, {"h": "ч", "d": "дн", "w": "нед"}[u])


class Bot:
    def __init__(self, tg, chat, name):
        self.tg, self.chat, self.name = tg, str(chat), name
        self.wait = {}  # chat -> ожидаемый ввод: "name"

    # ---------- команды ----------
    def cmd_users(self, chat):
        us = users()
        if not us:
            return self.tg.send(chat, "Пользователей пока нет. Добавить: /add")
        now = time.time()
        lines, kb, row = [], [], []
        for u in us:
            sc = u.get("sc") or u.get("status_code") or ""
            hs = int(u.get("hs") or 0)
            st = "🟢 в сети" if sc == "active" else ("⚪️ был " + ago(now - hs) if hs > 0 else "⚪️ не подключался")
            exp = u.get("expires_at")
            ex = ""
            if exp:
                left = int(exp) - now
                ex = " · ⏳ " + ("истёк" if left <= 0 else ("%d ч" % (left // 3600 + 1) if left < 86400 else "%d дн" % (left // 86400 + 1)))
            lines.append("<b>%s</b> — %s · %s%s" % (esc(u["name"]), st, fmt_bytes(int(u.get("rx", 0)) + int(u.get("tx", 0))), ex))
            row.append({"text": u["name"], "callback_data": "get:" + u["name"]})
            if len(row) == 3:
                kb.append(row)
                row = []
        if row:
            kb.append(row)
        self.tg.send(chat, "Пользователи (%d):\n%s\n\nНажмите на имя, чтобы получить QR-код и файл." % (len(us), "\n".join(lines)), kb)

    def send_client(self, chat, name, note=""):
        conf = os.path.join(A, name + ".conf")
        if not os.path.exists(conf):
            return self.tg.send(chat, "Файл подключения %s на сервере не найден." % esc(name))
        png = os.path.join(A, name + ".png")
        cap = (note + "\n" if note else "") + "<b>%s</b> — отсканируйте в AmneziaVPN или AmneziaWG." % esc(name)
        if os.path.exists(png):
            self.tg.call("sendPhoto", {"chat_id": chat, "caption": cap, "parse_mode": "HTML"}, {"photo": png})
        else:
            self.tg.send(chat, cap)
        self.tg.call("sendDocument", {"chat_id": chat}, {"document": conf})
        uri = os.path.join(A, name + ".vpnuri")
        if os.path.exists(uri):
            u = open(uri, encoding="utf-8").read().strip()
            if 0 < len(u) < 3800:
                self.tg.send(chat, "Ссылка для импорта:\n<code>%s</code>" % esc(u))

    def do_add(self, chat, name, expiry):
        # срок приходит и из данных кнопки, которые можно подделать: проверяем снова
        expiry = parse_expiry(expiry)
        if expiry is None:
            return self.tg.send(chat, "Не понял срок. Примеры: 30d, 2w, 12h или число дней.")
        if not NAME_RE.match(name) or name in RESERVED:
            return self.tg.send(chat, "Имя: латинские буквы, цифры, _ и - (до 32 символов). Попробуйте ещё раз: /add")
        if exists(name):
            return self.tg.send(chat, "Пользователь %s уже есть. Его QR-код: /get %s" % (esc(name), esc(name)))
        self.tg.send(chat, "Создаю пользователя %s…" % esc(name))
        aip = ""
        try:
            if os.path.getsize(EXCL_ALLOWED) > 0:
                aip = "--allowed-ips=" + open(EXCL_ALLOWED, encoding="utf-8").read().strip()
        except OSError:
            pass
        cmd = "bash %s --yes --no-color%s%s add %s" % (
            MANAGE, (" --expires=" + shlex.quote(expiry)) if expiry else "", (" " + shlex.quote(aip)) if aip else "", shlex.quote(name))
        rc, out, err = sh(cmd)
        if rc != 0 or not exists(name):
            tail = "\n".join((out + err).strip().splitlines()[-4:])
            log("add %s: ошибка %s" % (name, tail))
            return self.tg.send(chat, "Не удалось создать пользователя %s:\n<code>%s</code>" % (esc(name), esc(tail[-800:])))
        log("add %s (срок: %s)" % (name, expiry or "бессрочно"))
        self.send_client(chat, name, "✅ Пользователь создан" + (" на %s." % human_expiry(expiry) if expiry else " (бессрочно)."))

    def ask_delete(self, chat, name):
        if not exists(name) or name in RESERVED:
            return self.tg.send(chat, "Пользователя %s нет. Список: /users" % esc(name))
        self.tg.send(chat, "Удалить пользователя <b>%s</b>? Его ключ сразу перестанет работать." % esc(name),
                     [[{"text": "🗑 Удалить", "callback_data": "del:" + name}, {"text": "Отмена", "callback_data": "cancel"}]])

    def do_delete(self, chat, name):
        if not exists(name) or name in RESERVED:
            return self.tg.send(chat, "Пользователя %s уже нет." % esc(name))
        rc, out, err = sh("bash %s --yes --no-color remove %s" % (MANAGE, shlex.quote(name)))
        if exists(name):
            return self.tg.send(chat, "Не удалось удалить %s:\n<code>%s</code>" % (esc(name), esc((out + err).strip()[-800:])))
        log("del %s" % name)
        self.tg.send(chat, "🗑 Пользователь %s удалён." % esc(name))

    def cmd_proxy(self, chat):
        kv = {}
        try:
            for line in open(A + "/awgc-mtproto.conf", encoding="utf-8"):
                if "=" in line:
                    k, v = line.strip().split("=", 1)
                    kv[k] = v
        except OSError:
            pass
        if not kv.get("SECRET_SAVED"):
            return self.tg.send(chat, "MTProto-прокси не включён. Включить: программа → «Эксперименты» → «MTProto-прокси».")
        link = "tg://proxy?server=%s&port=%s&secret=%s" % (kv.get("HOST_SAVED"), kv.get("PORT_SAVED"), kv["SECRET_SAVED"])
        web = "https://t.me/proxy?server=%s&port=%s&secret=%s" % (kv.get("HOST_SAVED"), kv.get("PORT_SAVED"), kv["SECRET_SAVED"])
        self.tg.send(chat, "MTProto-прокси для Telegram. Откройте ссылку на телефоне — Telegram предложит подключить прокси:\n%s\n\n<code>%s</code>" % (esc(web), esc(link)))

    def cmd_status(self, chat):
        def active(u):
            return subprocess.run(["systemctl", "is-active", "--quiet", u]).returncode == 0
        lines = ["VPN на сервере RU: " + ("✅ работает" if active("awg-quick@awg0") else "❌ не работает")]
        if os.path.exists("/etc/amnezia/amneziawg/awg1.conf"):
            rc, out, _ = sh("awg show awg1 latest-handshakes 2>/dev/null | awk 'NR==1{print $2}'", 20)
            hs = int(out.strip() or 0)
            age = time.time() - hs if hs else -1
            ok = 0 <= age <= 300
            lines.append("Туннель к зарубежному серверу: " + (("✅ связь " + ago(age)) if ok else ("❌ нет связи" + ("" if age < 0 else " " + ago(age)))))
            rc, out, _ = sh("curl -s -m 12 --interface awg1 -o /dev/null -w '%{http_code}' https://www.gstatic.com/generate_204", 20)
            lines.append("Зарубежный трафик: " + ("✅ проходит" if out.strip() == "204" else "❌ не проходит"))
        us = users()
        lines.append("Пользователей: %d, в сети: %d" % (len(us), sum(1 for u in us if (u.get("sc") or u.get("status_code")) == "active")))
        self.tg.send(chat, "\n".join(lines))

    # ---------- разбор входящих ----------
    def on_message(self, m):
        chat = str(m["chat"]["id"])
        text = (m.get("text") or "").strip()
        if not text:
            return
        if text.startswith("/"):
            self.wait.pop(chat, None)
            parts = text.split()
            cmd = parts[0].split("@")[0].lower()
            args = parts[1:]
            if cmd in ("/start", "/help"):
                return self.tg.send(chat, "Каскад «%s».\n\n%s" % (esc(self.name), HELP))
            if cmd == "/users":
                return self.cmd_users(chat)
            if cmd == "/status":
                return self.cmd_status(chat)
            if cmd == "/proxy":
                return self.cmd_proxy(chat)
            if cmd == "/get":
                if not args:
                    return self.cmd_users(chat)
                if not exists(args[0]) or args[0] in RESERVED:
                    return self.tg.send(chat, "Пользователя %s нет. Список: /users" % esc(args[0]))
                return self.send_client(chat, args[0])
            if cmd == "/del":
                if not args:
                    return self.tg.send(chat, "Укажите имя: /del имя")
                return self.ask_delete(chat, args[0])
            if cmd == "/add":
                if not args:
                    self.wait[chat] = "name"
                    return self.tg.send(chat, "Как назвать пользователя? Латинские буквы, цифры, _ и - (например, phone_anna).")
                exp = parse_expiry(args[1] if len(args) > 1 else "")
                if exp is None:
                    return self.tg.send(chat, "Не понял срок. Примеры: 30d, 2w, 12h или число дней.")
                if len(args) > 1:
                    return self.do_add(chat, args[0], exp)
                return self.ask_expiry(chat, args[0])
            return self.tg.send(chat, HELP)
        if self.wait.get(chat) == "name":
            self.wait.pop(chat, None)
            name = text
            if not NAME_RE.match(name) or name in RESERVED:
                self.wait[chat] = "name"
                return self.tg.send(chat, "Имя: только латинские буквы, цифры, _ и - (до 32 символов). Как назвать пользователя?")
            if exists(name):
                return self.tg.send(chat, "Пользователь %s уже есть. Его QR-код: /get %s" % (esc(name), esc(name)))
            return self.ask_expiry(chat, name)
        self.tg.send(chat, HELP)

    def ask_expiry(self, chat, name):
        if not NAME_RE.match(name) or name in RESERVED:
            return self.tg.send(chat, "Имя: латинские буквы, цифры, _ и - (до 32 символов).")
        if exists(name):
            return self.tg.send(chat, "Пользователь %s уже есть. Его QR-код: /get %s" % (esc(name), esc(name)))
        kb = [[{"text": t, "callback_data": "add:%s:%s" % (name, e)} for t, e in EXPIRY_BUTTONS[:3]],
              [{"text": t, "callback_data": "add:%s:%s" % (name, e)} for t, e in EXPIRY_BUTTONS[3:]]]
        self.tg.send(chat, "Срок действия для <b>%s</b>?" % esc(name), kb)

    def on_callback(self, q):
        chat = str(q["message"]["chat"]["id"])
        data = q.get("data") or ""
        try:
            self.tg.call("answerCallbackQuery", {"callback_query_id": q["id"]})
            self.tg.call("editMessageReplyMarkup", {"chat_id": chat, "message_id": q["message"]["message_id"], "reply_markup": {"inline_keyboard": []}})
        except RuntimeError:
            pass
        if data.startswith("get:"):
            n = data[4:]
            if exists(n) and n not in RESERVED:
                return self.send_client(chat, n)
            return self.tg.send(chat, "Пользователя %s больше нет." % esc(n))
        if data.startswith("add:"):
            _, n, e = (data.split(":", 2) + [""])[:3]
            return self.do_add(chat, n, e)
        if data.startswith("del:"):
            return self.do_delete(chat, data[4:])
        if data == "cancel":
            return self.tg.send(chat, "Отменено.")

    def handle(self, upd):
        m = upd.get("message")
        q = upd.get("callback_query")
        src = (m or (q or {}).get("message") or {}).get("chat", {}).get("id")
        if src is None or str(src) != self.chat:
            return  # чужие чаты не обслуживаем
        try:
            if m:
                self.on_message(m)
            elif q:
                self.on_callback(q)
        except Exception as e:  # бот не должен падать из-за одной команды
            log("ошибка: %r" % e)
            try:
                self.tg.send(self.chat, "Ошибка: %s" % esc(e))
            except RuntimeError:
                pass


def main():
    kv = read_conf()
    token, chat = kv.get("TG_TOKEN", ""), kv.get("TG_CHAT", "")
    if not token or not chat:
        log("нет токена или chat id — выход")
        sys.exit(0)
    tg = TG(token)
    bot = Bot(tg, chat, kv.get("NAME") or "AmneziaWG")
    try:
        tg.call("setMyCommands", {"commands": [
            {"command": "users", "description": "Пользователи"},
            {"command": "add", "description": "Добавить пользователя"},
            {"command": "get", "description": "QR-код и файл подключения"},
            {"command": "del", "description": "Удалить пользователя"},
            {"command": "status", "description": "Состояние каскада"},
            {"command": "proxy", "description": "Ссылка на MTProto-прокси"},
            {"command": "help", "description": "Помощь"}]})
    except RuntimeError as e:
        log("setMyCommands: %s" % e)
    log("бот запущен")
    offset, fails = 0, 0
    while True:
        try:
            ups = tg.call("getUpdates", {"timeout": 50, "offset": offset, "allowed_updates": ["message", "callback_query"]}, timeout=65)
            fails = 0
        except RuntimeError as e:
            fails += 1
            if "Conflict" in str(e):  # кто-то ещё читает обновления (кнопка «Определить»)
                time.sleep(15)
            else:
                log("getUpdates: %s" % e)
                time.sleep(min(60, 5 * fails))
            continue
        for u in ups or []:
            offset = max(offset, u.get("update_id", 0) + 1)
            bot.handle(u)


if __name__ == "__main__":
    main()
