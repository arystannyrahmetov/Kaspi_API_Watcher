// Публикация Swagger на хостинге по FTP.
//
// Собирает одностраничный HTML со Swagger UI, внутрь которого целиком зашита
// свежая спецификация (рядом кладётся и сам openapi.json — чтобы его можно
// было скачать и открыть в Postman), и заливает всё это по FTP.
//
// Настройки берутся из переменных окружения (их удобно задать в run.bat)
// или из флагов командной строки:
//
//	FTP_HOST   хост, можно с портом: ftp.example.kz или ftp.example.kz:21
//	FTP_DIR    каталог на хостинге, например /public_html/kaspi
//	FTP_FILE   имя файла, например kaspi-api.html
//	FTP_USER   логин
//	FTP_PASS   пароль
//	FTP_TLS    off (по умолчанию) | explicit (AUTH TLS) | implicit (порт 990)
//	PUBLIC_URL адрес страницы — уйдёт ссылкой в уведомление
//
// Внешних зависимостей нет: FTP-клиент написан здесь же на net + crypto/tls.
package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// Страница со Swagger UI
// ============================================================================

const swaggerCDN = "https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14"

// swaggerHTML собирает самодостаточную страницу: спецификация зашита в сам
// HTML, поэтому браузеру не нужны ни CORS, ни правильный MIME у json-файла.
func swaggerHTML(spec *OMap, jsonName, stamp string) ([]byte, error) {
	var specBuf bytes.Buffer
	enc := json.NewEncoder(&specBuf)
	enc.SetIndent("", " ")
	if err := enc.Encode(spec); err != nil {
		return nil, err
	}
	// Внутри <script> не должно остаться ни одного "<": иначе "</script>",
	// попавшийся в примере ответа, порвал бы страницу. Энкодер такое
	// экранирует и сам, но здесь от этого зависит целостность разметки,
	// поэтому подстрахуемся явно. В JSON < > & встречаются только внутри
	// строк, так что замена по всему тексту безопасна.
	embedded := jsonHTMLEscape.Replace(specBuf.String())

	title := "Kaspi Shop API"
	if info, ok := spec.Get("info"); ok {
		if im, ok := info.(*OMap); ok {
			if t, ok := im.Get("title"); ok {
				if s, ok := t.(string); ok && s != "" {
					title = s
				}
			}
		}
	}

	download := ""
	if jsonName != "" {
		download = `<a class="dl" href="` + htmlAttr(jsonName) + `" download>openapi.json</a>`
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, `<!DOCTYPE html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>%s</title>
<link rel="stylesheet" href="%s/swagger-ui.css">
<style>
  body { margin: 0; background: #fafafa; }
  .topbar { display: flex; align-items: center; gap: 12px; flex-wrap: wrap;
            padding: 10px 18px; background: #1b1b1b; color: #fff;
            font: 14px/1.4 -apple-system, "Segoe UI", Roboto, Arial, sans-serif; }
  .topbar b { font-size: 15px; }
  .topbar .stamp { color: #9e9e9e; }
  .topbar .dl { margin-left: auto; color: #6fd08c; text-decoration: none;
                border: 1px solid #6fd08c; border-radius: 4px; padding: 3px 10px; }
  .topbar .dl:hover { background: #6fd08c; color: #1b1b1b; }
  .swagger-ui .topbar { display: none; }
</style>
</head>
<body>
<div class="topbar"><b>%s</b><span class="stamp">обновлено %s</span>%s</div>
<div id="swagger-ui"></div>
<script id="openapi-spec" type="application/json">
%s</script>
<script src="%s/swagger-ui-bundle.js" crossorigin></script>
<script src="%s/swagger-ui-standalone-preset.js" crossorigin></script>
<script>
window.ui = SwaggerUIBundle({
  spec: JSON.parse(document.getElementById('openapi-spec').textContent),
  dom_id: '#swagger-ui',
  deepLinking: true,
  docExpansion: 'list',
  defaultModelsExpandDepth: 1,
  tryItOutEnabled: false,
  presets: [SwaggerUIBundle.presets.apis, SwaggerUIStandalonePreset],
  plugins: [SwaggerUIBundle.plugins.DownloadUrl],
  layout: 'BaseLayout'
});
</script>
</body>
</html>
`, htmlAttr(title), swaggerCDN, htmlAttr(title), htmlAttr(stamp), download,
		embedded, swaggerCDN, swaggerCDN)
	return b.Bytes(), nil
}

// jsonHTMLEscape прячет символы, опасные внутри <script>, в escape-последовательности
// вида \u003c. Собираем их из кусочков, чтобы в исходнике не было соблазна
// «поправить» экранирование.
const backslash = "\\"

var jsonHTMLEscape = strings.NewReplacer(
	"<", backslash+"u003c",
	">", backslash+"u003e",
	"&", backslash+"u0026",
)

var htmlAttrRepl = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")

func htmlAttr(s string) string { return htmlAttrRepl.Replace(s) }

// ============================================================================
// Публикация
// ============================================================================

// publish собирает страницу и заливает её (и openapi.json) на хостинг.
// Возвращает true, если файлы действительно ушли на сервер.
func publish(cfg *Config, spec *OMap, specPath string) bool {
	if cfg.FTPHost == "" {
		return false
	}
	if cfg.FTPUser == "" || cfg.FTPPass == "" {
		log.Printf("FTP: задан FTP_HOST, но нет FTP_USER/FTP_PASS — публикация пропущена")
		return false
	}
	name := path.Base(strings.ReplaceAll(cfg.FTPFile, "\\", "/"))
	if name == "" || name == "." || name == "/" {
		name = "kaspi-api.html"
	}
	jsonName := ""
	if cfg.FTPJSON {
		jsonName = strings.TrimSuffix(name, filepath.Ext(name)) + ".json"
	}

	specBytes, err := os.ReadFile(specPath)
	if err != nil {
		log.Printf("FTP: не могу прочитать %s: %v", specPath, err)
		return false
	}
	page, err := swaggerHTML(spec, jsonName, time.Now().Format("02.01.2006 15:04"))
	if err != nil {
		log.Printf("FTP: не удалось собрать HTML: %v", err)
		return false
	}

	// Не гоняем файл впустую: сверяемся с тем, что залили в прошлый раз.
	cache := filepath.Join(cfg.Dir, "published.json")
	if !cfg.FTPAlways {
		if old, err := os.ReadFile(cache); err == nil && bytes.Equal(old, specBytes) {
			log.Printf("FTP: на хостинге уже актуальная версия, заливать нечего")
			return false
		}
	}

	type upload struct {
		name string
		data []byte
	}
	files := []upload{{name, page}}
	if jsonName != "" {
		files = append(files, upload{jsonName, specBytes})
	}

	if cfg.DryRun {
		// Кладём рядом оба файла — получается ровно то, что увидел бы хостинг,
		// и ссылку на openapi.json можно проверить прямо на месте.
		for _, f := range files {
			local := filepath.Join(cfg.Dir, f.name)
			if err := os.WriteFile(local, f.data, 0o644); err != nil {
				log.Printf("FTP: не могу записать %s: %v", local, err)
				return false
			}
		}
		log.Printf("FTP: dry-run, ничего не заливаю. Страница лежит здесь: %s",
			filepath.Join(cfg.Dir, name))
		return false
	}

	c, err := ftpDial(cfg)
	if err != nil {
		log.Printf("FTP: не подключиться к %s: %v", cfg.FTPHost, err)
		return false
	}
	defer c.quit()

	if cfg.FTPDir != "" {
		if err := c.chdir(cfg.FTPDir); err != nil {
			log.Printf("FTP: каталог %s недоступен: %v", cfg.FTPDir, err)
			return false
		}
	}
	for _, f := range files {
		if err := c.store(f.name, f.data); err != nil {
			log.Printf("FTP: файл %s не залит: %v", f.name, err)
			return false
		}
		log.Printf("FTP: залито %s (%d КБ)", f.name, (len(f.data)+1023)/1024)
	}
	if err := os.WriteFile(cache, specBytes, 0o644); err != nil {
		log.Printf("FTP: не могу запомнить залитую версию: %v", err)
	}
	if cfg.PublicURL != "" {
		log.Printf("FTP: страница доступна по адресу %s", cfg.PublicURL)
	}
	return true
}

// ============================================================================
// Минимальный FTP-клиент (RFC 959 + AUTH TLS из RFC 4217)
// ============================================================================

type ftpClient struct {
	conn net.Conn
	r    *bufio.Reader
	tls  *tls.Config // не nil, если канал шифруется — тогда шифруем и данные
	host string
}

func ftpDial(cfg *Config) (*ftpClient, error) {
	mode := strings.ToLower(strings.TrimSpace(cfg.FTPTLS))
	switch mode {
	case "", "0", "no", "off", "false", "none":
		mode = "off"
	case "1", "yes", "true", "explicit", "auth", "ftps", "tls":
		mode = "explicit"
	case "implicit", "990":
		mode = "implicit"
	default:
		return nil, fmt.Errorf("непонятное FTP_TLS=%q (нужно off, explicit или implicit)", cfg.FTPTLS)
	}

	host := strings.TrimSpace(cfg.FTPHost)
	host = strings.TrimPrefix(strings.TrimPrefix(host, "ftp://"), "ftps://")
	host = strings.TrimSuffix(host, "/")
	if _, _, err := net.SplitHostPort(host); err != nil {
		port := "21"
		if mode == "implicit" {
			port = "990"
		}
		host = net.JoinHostPort(host, port)
	}
	bare, _, _ := net.SplitHostPort(host)

	var tlsCfg *tls.Config
	if mode != "off" {
		tlsCfg = &tls.Config{
			ServerName:         bare,
			InsecureSkipVerify: cfg.FTPSkipVerify,
			// Некоторые серверы требуют, чтобы канал данных переиспользовал
			// TLS-сессию управляющего соединения.
			ClientSessionCache: tls.NewLRUClientSessionCache(8),
		}
	}

	conn, err := net.DialTimeout("tcp", host, 30*time.Second)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(10 * time.Minute))
	if mode == "implicit" {
		conn = tls.Client(conn, tlsCfg)
	}
	c := &ftpClient{conn: conn, r: bufio.NewReader(conn), tls: tlsCfg, host: bare}

	if code, msg, err := c.readResp(); err != nil || code != 220 {
		conn.Close()
		if err != nil {
			return nil, fmt.Errorf("приветствие сервера: %w", err)
		}
		return nil, fmt.Errorf("приветствие сервера: %d %s", code, oneLine(msg))
	}
	if mode == "explicit" {
		if err := c.must(234, "AUTH TLS"); err != nil {
			conn.Close()
			return nil, fmt.Errorf("AUTH TLS: %w", err)
		}
		tc := tls.Client(conn, tlsCfg)
		if err := tc.Handshake(); err != nil {
			conn.Close()
			return nil, fmt.Errorf("рукопожатие TLS: %w", err)
		}
		c.conn, c.r = tc, bufio.NewReader(tc)
	}
	if c.tls != nil {
		// Без PBSZ/PROT сервер обычно отвечает на STOR ошибкой 522/534.
		if err := c.must(200, "PBSZ 0"); err != nil {
			c.quit()
			return nil, fmt.Errorf("PBSZ: %w", err)
		}
		if err := c.must(200, "PROT P"); err != nil {
			c.quit()
			return nil, fmt.Errorf("PROT: %w", err)
		}
	}

	code, msg, err := c.send("USER %s", cfg.FTPUser)
	if err != nil {
		c.quit()
		return nil, fmt.Errorf("USER: %w", err)
	}
	switch {
	case code == 331 || code == 332: // сервер просит пароль
		if err := c.must(230, "PASS %s", cfg.FTPPass); err != nil {
			c.quit()
			return nil, fmt.Errorf("вход не удался (проверьте логин и пароль): %w", err)
		}
	case code == 230: // вошли без пароля
	default:
		c.quit()
		return nil, fmt.Errorf("USER: %d %s", code, oneLine(msg))
	}
	if err := c.must(200, "TYPE I"); err != nil {
		c.quit()
		return nil, fmt.Errorf("TYPE I: %w", err)
	}
	return c, nil
}

// readResp читает ответ сервера, включая многострочный ("250-" … "250 ").
func (c *ftpClient) readResp() (int, string, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return 0, "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) < 4 {
		return 0, line, fmt.Errorf("непонятный ответ %q", line)
	}
	code, err := strconv.Atoi(line[:3])
	if err != nil {
		return 0, line, fmt.Errorf("непонятный ответ %q", line)
	}
	msg := line[4:]
	if line[3] == '-' {
		end := line[:3] + " "
		for {
			l, err := c.r.ReadString('\n')
			if err != nil {
				return code, msg, err
			}
			l = strings.TrimRight(l, "\r\n")
			msg += "\n" + l
			if strings.HasPrefix(l, end) {
				break
			}
		}
	}
	return code, msg, nil
}

func (c *ftpClient) send(format string, args ...any) (int, string, error) {
	if _, err := fmt.Fprintf(c.conn, format+"\r\n", args...); err != nil {
		return 0, "", err
	}
	return c.readResp()
}

// must отправляет команду и требует ответ ожидаемого кода (или того же класса).
func (c *ftpClient) must(expect int, format string, args ...any) error {
	code, msg, err := c.send(format, args...)
	if err != nil {
		return err
	}
	if code != expect && code/100 != expect/100 {
		return fmt.Errorf("%d %s", code, oneLine(msg))
	}
	return nil
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	return s
}

// chdir переходит в каталог, создавая недостающие уровни.
func (c *ftpClient) chdir(dir string) error {
	dir = strings.ReplaceAll(strings.TrimSpace(dir), "\\", "/")
	if strings.HasPrefix(dir, "/") {
		if err := c.must(250, "CWD /"); err != nil {
			return err
		}
	}
	for _, part := range strings.Split(dir, "/") {
		if part == "" || part == "." {
			continue
		}
		if err := c.must(250, "CWD %s", part); err != nil {
			if e := c.must(257, "MKD %s", part); e != nil {
				return fmt.Errorf("каталога %s нет, и создать не вышло: %v", part, e)
			}
			if err := c.must(250, "CWD %s", part); err != nil {
				return err
			}
		}
	}
	return nil
}

// store заливает файл во временное имя и переименовывает — чтобы посетитель
// сайта не поймал наполовину записанную страницу.
func (c *ftpClient) store(name string, data []byte) error {
	tmp := name + ".part"
	dc, err := c.dataConn()
	if err != nil {
		return err
	}
	code, msg, err := c.send("STOR %s", tmp)
	if err != nil || (code != 125 && code != 150) {
		dc.Close()
		if err != nil {
			return fmt.Errorf("STOR: %w", err)
		}
		return fmt.Errorf("STOR: %d %s", code, oneLine(msg))
	}
	if _, err := io.Copy(dc, bytes.NewReader(data)); err != nil {
		dc.Close()
		return err
	}
	if err := dc.Close(); err != nil { // для TLS закрытие шлёт close_notify
		return err
	}
	if code, msg, err := c.readResp(); err != nil {
		return fmt.Errorf("завершение передачи: %w", err)
	} else if code/100 != 2 {
		return fmt.Errorf("завершение передачи: %d %s", code, oneLine(msg))
	}
	c.send("DELE %s", name) // старого файла может и не быть — не страшно
	if err := c.must(350, "RNFR %s", tmp); err != nil {
		return fmt.Errorf("RNFR: %w", err)
	}
	if err := c.must(250, "RNTO %s", name); err != nil {
		return fmt.Errorf("RNTO: %w", err)
	}
	return nil
}

// dataConn открывает пассивное соединение: сначала EPSV, при отказе PASV.
func (c *ftpClient) dataConn() (net.Conn, error) {
	addr, err := c.epsv()
	if err != nil {
		var perr error
		addr, perr = c.pasv()
		if perr != nil {
			return nil, fmt.Errorf("пассивный режим: EPSV — %v; PASV — %v", err, perr)
		}
	}
	conn, err := net.DialTimeout("tcp", addr, 30*time.Second)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(10 * time.Minute))
	if c.tls != nil {
		conn = tls.Client(conn, c.tls)
	}
	return conn, nil
}

func (c *ftpClient) epsv() (string, error) {
	code, msg, err := c.send("EPSV")
	if err != nil {
		return "", err
	}
	if code != 229 {
		return "", fmt.Errorf("%d %s", code, oneLine(msg))
	}
	// 229 Entering Extended Passive Mode (|||51234|)
	open := strings.Index(msg, "(")
	closing := strings.LastIndex(msg, ")")
	if open < 0 || closing <= open+1 {
		return "", fmt.Errorf("непонятный ответ %q", oneLine(msg))
	}
	sep := string(msg[open+1])
	parts := strings.Split(msg[open+1:closing], sep)
	if len(parts) < 4 {
		return "", fmt.Errorf("непонятный ответ %q", oneLine(msg))
	}
	port := strings.TrimSpace(parts[len(parts)-2])
	if n, err := strconv.Atoi(port); err != nil || n <= 0 || n > 65535 {
		return "", fmt.Errorf("непонятный порт %q", port)
	}
	return net.JoinHostPort(c.host, port), nil
}

func (c *ftpClient) pasv() (string, error) {
	code, msg, err := c.send("PASV")
	if err != nil {
		return "", err
	}
	if code != 227 {
		return "", fmt.Errorf("%d %s", code, oneLine(msg))
	}
	// 227 Entering Passive Mode (h1,h2,h3,h4,p1,p2)
	open, closing := strings.LastIndex(msg, "("), strings.LastIndex(msg, ")")
	if open < 0 || closing <= open {
		open, closing = strings.LastIndex(msg, " "), len(msg)
	}
	nums := strings.Split(strings.TrimSpace(msg[open+1:closing]), ",")
	if len(nums) < 6 {
		return "", fmt.Errorf("непонятный ответ %q", oneLine(msg))
	}
	nums = nums[len(nums)-6:]
	p1, err1 := strconv.Atoi(strings.TrimSpace(nums[4]))
	p2, err2 := strconv.Atoi(strings.TrimSpace(nums[5]))
	if err1 != nil || err2 != nil {
		return "", fmt.Errorf("непонятный порт в %q", oneLine(msg))
	}
	// Адрес из ответа часто внутренний (NAT у хостера), поэтому берём тот,
	// к которому уже подключены, а из PASV — только порт.
	return net.JoinHostPort(c.host, strconv.Itoa(p1*256+p2)), nil
}

func (c *ftpClient) quit() {
	if c.conn != nil {
		c.send("QUIT")
		c.conn.Close()
	}
}
