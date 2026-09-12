@echo off
chcp 65001 >nul
cd /d "%~dp0"

rem ============================================================
rem  Значения пишутся сразу после знака "=", без кавычек
rem  и без пробела до и после "=" — пробел попадёт в значение.
rem ============================================================

rem ---------- Telegram ----------
set TELEGRAM_BOT_TOKEN=
set TELEGRAM_CHAT_ID=

rem ---------- Магазин ----------
rem X-Merchant-Uid — обязательный заголовок, которого нет в документации.
rem Укажите здесь код магазина — он подставится в схему примером ко всем
rem запросам, и в Swagger/Bruno его не придётся вписывать руками.
rem В одном кабинете несколько магазинов — ставьте тот, с которым работаете.
set MERCHANT_UID={{merchantUid}}

rem ---------- Хостинг (FTP): куда класть Swagger ----------
rem FTP_HOST  — хост, можно с портом:  ftp.example.kz  или  ftp.example.kz:21
rem FTP_DIR   — каталог на хостинге:   /public_html/kaspi   (создаётся сам)
rem FTP_FILE  — имя html-файла:        kaspi-api.html
rem FTP_USER  — логин FTP
rem FTP_PASS  — пароль FTP
rem Оставьте FTP_HOST пустым — публикация просто не будет выполняться.
set FTP_HOST=
set FTP_DIR=
set FTP_FILE=
set FTP_USER=
set FTP_PASS=

rem Адрес получившейся страницы — придёт ссылкой в Telegram (необязательно)
set PUBLIC_URL=

rem ---------- Дополнительно (менять обычно не нужно) ----------
rem FTP_TLS         off | explicit (FTPS, AUTH TLS) | implicit (порт 990)
rem FTP_INSECURE    1 — не проверять TLS-сертификат хостинга
rem FTP_UPLOAD_JSON 1 — класть рядом с html и сам openapi.json (по умолчанию да)
rem FTP_ALWAYS      1 — заливать каждый прогон, даже если ничего не изменилось
set FTP_TLS=off
set FTP_INSECURE=0
set FTP_UPLOAD_JSON=1
set FTP_ALWAYS=0

rem ============================================================
rem  Аргументы запуска можно дописать при вызове run.bat, например:
rem     run.bat -interval 60     проверять раз в час
rem     run.bat -dry-run         ничего не отправлять и не заливать
rem ============================================================
"%~dp0kaspi-watcher.exe" %*
