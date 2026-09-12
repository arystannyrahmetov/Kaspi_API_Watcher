#!/bin/sh
# Шаблон запуска под Linux и macOS.
#
#   cp run-example.sh run.sh && chmod +x run.sh
#
# Заполните значения и запускайте ./run.sh — файл run.sh перечислен
# в .gitignore, потому что в нём оказываются токен бота и пароль от FTP.
#
# Значения пишутся в кавычках, знак "=" без пробелов по бокам.

set -eu
cd "$(dirname "$0")"

# ---------- Telegram ----------
export TELEGRAM_BOT_TOKEN=""
export TELEGRAM_CHAT_ID=""

# ---------- Магазин ----------
# X-Merchant-Uid — обязательный заголовок, которого нет в документации.
# Укажите здесь код магазина — он подставится в схему примером ко всем
# запросам, и в Swagger/Bruno его не придётся вписывать руками.
# В одном кабинете несколько магазинов — ставьте тот, с которым работаете.
export MERCHANT_UID="{{merchantUid}}"

# ---------- Хостинг (FTP): куда класть Swagger ----------
# FTP_HOST  — хост, можно с портом:  ftp.example.kz  или  ftp.example.kz:21
# FTP_DIR   — каталог на хостинге:   /public_html/kaspi   (создаётся сам)
# FTP_FILE  — имя html-файла:        kaspi-api.html
# FTP_USER  — логин FTP
# FTP_PASS  — пароль FTP
# Оставьте FTP_HOST пустым — публикация просто не будет выполняться.
export FTP_HOST=""
export FTP_DIR=""
export FTP_FILE=""
export FTP_USER=""
export FTP_PASS=""

# Адрес получившейся страницы — придёт ссылкой в Telegram (необязательно)
export PUBLIC_URL=""

# ---------- Дополнительно (менять обычно не нужно) ----------
# FTP_TLS         off | explicit (FTPS, AUTH TLS) | implicit (порт 990)
# FTP_INSECURE    1 — не проверять TLS-сертификат хостинга
# FTP_UPLOAD_JSON 1 — класть рядом с html и сам openapi.json (по умолчанию да)
# FTP_ALWAYS      1 — заливать каждый прогон, даже если ничего не изменилось
export FTP_TLS="off"
export FTP_INSECURE="0"
export FTP_UPLOAD_JSON="1"
export FTP_ALWAYS="0"

# Папка для данных. По умолчанию ./kaspi_watch рядом со скриптом; в cron
# и systemd лучше указать абсолютный путь.
# export KASPI_WATCH_DIR="/var/lib/kaspi-watcher"

# ============================================================
#  Аргументы дописываются при вызове, например:
#     ./run.sh -interval 60     проверять раз в час
#     ./run.sh -dry-run         ничего не отправлять и не заливать
# ============================================================
exec ./kaspi-watcher "$@"
