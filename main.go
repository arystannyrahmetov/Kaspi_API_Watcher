// Kaspi API Guide Watcher
//
// Следит за документацией API Магазина на Kaspi.kz в Kaspi Гид:
//
//	https://guide.kaspi.kz/partner/ru/shop/api/orders
//	https://guide.kaspi.kz/partner/ru/shop/api/goods
//
// собирает из неё OpenAPI 3 (Swagger) и присылает уведомление, если на сайте
// что-то поменялось: добавили или удалили вопрос, изменили текст, таблицы,
// примеры запросов и ответов.
//
// Браузер не нужен: сервер отдаёт все вопросы раздела в JSON внутри страницы
// (<script id="__NUXT_DATA__">), поэтому на раздел уходит один GET-запрос.
// Только стандартная библиотека Go, внешних зависимостей нет.
//
// Сборка:
//
//	go build -o kaspi-watcher .
//
// Переменные окружения:
//
//	TELEGRAM_BOT_TOKEN  токен бота от @BotFather
//	TELEGRAM_CHAT_ID    id чата или канала
//	WEBHOOK_URL         (необязательно) Slack / Discord / Mattermost webhook
//	KASPI_WATCH_DIR     папка для данных (по умолчанию ./kaspi_watch)
//
// Публикация Swagger на своём хостинге (всё необязательно, см. publish.go):
//
//	FTP_HOST            хост, можно с портом: ftp.example.kz или ftp.example.kz:21
//	FTP_DIR             каталог на хостинге, например /public_html/kaspi
//	FTP_FILE            имя html-файла, по умолчанию kaspi-api.html
//	FTP_USER, FTP_PASS  логин и пароль
//	FTP_TLS             off (по умолчанию) | explicit | implicit
//	PUBLIC_URL          адрес страницы — придёт ссылкой в уведомление
//
// Все они же есть и флагами: -ftp-host, -ftp-dir, -ftp-file, -ftp-user,
// -ftp-pass, -ftp-tls, -public-url.
//
// Запуск:
//
//	./kaspi-watcher                 один прогон (для cron)
//	./kaspi-watcher -interval 60    работать самому, проверка раз в 60 минут
//	./kaspi-watcher -dry-run        ничего не отправлять, всё в консоль
//	./kaspi-watcher -from ./pages   читать orders.html / goods.html с диска
//
// Результат в KASPI_WATCH_DIR:
//
//	openapi.json     актуальный Swagger (editor.swagger.io, Postman, Bruno)
//	snapshot.json    снимок всех вопросов, с ним сравнивается следующий прогон
//	published.json   версия, залитая на хостинг, чтобы не заливать её заново
//	history/<время>/ дифф и предыдущие версии при каждом изменении
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	guideBase = "https://guide.kaspi.kz/partner/ru"
	apiServer = "https://kaspi.kz/shop"
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0 Safari/537.36"
)

var sectionTitles = map[string]string{"orders": "Заказы", "goods": "Товары", "general": "Общие вопросы"}

// ============================================================================
// Конфигурация
// ============================================================================

type Config struct {
	Dir      string
	Sections []string
	From     string
	Interval int
	DryRun   bool
	ExitCode bool
	TgToken  string
	TgChat   string
	Webhook  string

	// Значение X-Merchant-Uid для примеров в схеме (необязательно)
	MerchantUID string

	// Публикация Swagger на хостинге по FTP (см. publish.go)
	FTPHost       string
	FTPDir        string
	FTPFile       string
	FTPUser       string
	FTPPass       string
	FTPTLS        string
	FTPSkipVerify bool
	FTPJSON       bool
	FTPAlways     bool
	PublicURL     string
}

func loadConfig() *Config {
	c := &Config{
		TgToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		TgChat:  os.Getenv("TELEGRAM_CHAT_ID"),
		Webhook: os.Getenv("WEBHOOK_URL"),
	}
	defDir := os.Getenv("KASPI_WATCH_DIR")
	if defDir == "" {
		defDir = "./kaspi_watch"
	}
	var sections string
	flag.StringVar(&c.Dir, "dir", defDir, "папка для данных")
	flag.StringVar(&sections, "sections", "orders,goods", "разделы API через запятую")
	flag.StringVar(&c.From, "from", "", "читать страницы <раздел>.html из этой папки вместо сайта")
	flag.IntVar(&c.Interval, "interval", 0, "проверять в цикле раз в N минут (0 — один прогон)")
	flag.BoolVar(&c.DryRun, "dry-run", false, "не отправлять уведомления, печатать в консоль")
	flag.BoolVar(&c.ExitCode, "exit-code", false, "код выхода 1, если были изменения (для CI)")
	flag.StringVar(&c.MerchantUID, "merchant-uid", os.Getenv("MERCHANT_UID"),
		"идентификатор магазина: подставится в схему примером для заголовка "+merchantUIDHeader)

	// Публикация Swagger на хостинге. Значения по умолчанию — из окружения,
	// то есть из run.bat; флаги командной строки их перебивают.
	flag.StringVar(&c.FTPHost, "ftp-host", os.Getenv("FTP_HOST"), "FTP-хост, можно с портом (ftp.example.kz:21)")
	flag.StringVar(&c.FTPDir, "ftp-dir", os.Getenv("FTP_DIR"), "каталог на хостинге (/public_html/kaspi)")
	flag.StringVar(&c.FTPFile, "ftp-file", envOr("FTP_FILE", "kaspi-api.html"), "имя html-файла на хостинге")
	flag.StringVar(&c.FTPUser, "ftp-user", os.Getenv("FTP_USER"), "логин FTP")
	flag.StringVar(&c.FTPPass, "ftp-pass", os.Getenv("FTP_PASS"), "пароль FTP")
	flag.StringVar(&c.FTPTLS, "ftp-tls", envOr("FTP_TLS", "off"), "шифрование FTP: off | explicit | implicit")
	flag.BoolVar(&c.FTPSkipVerify, "ftp-insecure", envBool("FTP_INSECURE", false), "не проверять TLS-сертификат хостинга")
	flag.BoolVar(&c.FTPJSON, "ftp-json", envBool("FTP_UPLOAD_JSON", true), "класть рядом с html и сам openapi.json")
	flag.BoolVar(&c.FTPAlways, "ftp-always", envBool("FTP_ALWAYS", false), "заливать каждый прогон, даже если ничего не изменилось")
	flag.StringVar(&c.PublicURL, "public-url", os.Getenv("PUBLIC_URL"), "адрес страницы на хостинге — уйдёт ссылкой в уведомление")

	flag.Parse()
	for _, s := range strings.Split(sections, ",") {
		if s = strings.TrimSpace(s); s != "" {
			c.Sections = append(c.Sections, s)
		}
	}
	return c
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func envBool(name string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "yes", "y", "true", "on", "да":
		return true
	case "0", "no", "n", "false", "off", "нет":
		return false
	}
	return def
}

// ============================================================================
// Упорядоченная map — чтобы JSON-примеры и Swagger сохраняли порядок полей
// (обычная map в Go порядок теряет, и диффы бы «шумели»).
// ============================================================================

type OMap struct {
	keys []string
	vals map[string]any
}

func NewM() *OMap { return &OMap{vals: map[string]any{}} }

func (o *OMap) Set(k string, v any) *OMap {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
	return o
}

func (o *OMap) Get(k string) (any, bool) { v, ok := o.vals[k]; return v, ok }
func (o *OMap) Len() int                 { return len(o.keys) }

func (o *OMap) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := marshalNoEscape(k)
		b.Write(kb)
		b.WriteByte(':')
		vb, err := marshalNoEscape(o.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func writeJSON(path string, v any) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}

// ============================================================================
// Загрузка страниц и распаковка __NUXT_DATA__ (формат devalue)
// ============================================================================

var httpClient = &http.Client{Timeout: 60 * time.Second}

func fetch(u string) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9")
		resp, err := httpClient.Do(req)
		if err == nil {
			body, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			switch {
			case rerr != nil:
				err = rerr
			case resp.StatusCode != http.StatusOK:
				err = fmt.Errorf("HTTP %d", resp.StatusCode)
			default:
				return body, nil
			}
		}
		lastErr = err
		log.Printf("  %s: попытка %d не удалась: %v", u, attempt, err)
		time.Sleep(time.Duration(attempt*3) * time.Second)
	}
	return nil, fmt.Errorf("%s: %w", u, lastErr)
}

var reNuxt = regexp.MustCompile(`(?s)<script[^>]*\bid="__NUXT_DATA__"[^>]*>(.*?)</script>`)

// devalue: плоский массив, ссылки на значения — индексы в этом массиве,
// а ["Reactive", i] / ["Set", ...] и т.п. — служебные обёртки.
type reviver struct {
	arr  []any
	memo map[int]any
	busy map[int]bool
}

func (r *reviver) at(i int) any {
	if i < 0 || i >= len(r.arr) { // -1 undefined, -2 hole и т.п.
		return nil
	}
	if v, ok := r.memo[i]; ok {
		return v
	}
	if r.busy[i] {
		return nil
	}
	r.busy[i] = true
	v := r.revive(r.arr[i])
	delete(r.busy, i)
	r.memo[i] = v
	return v
}

func (r *reviver) ref(x any) any {
	n, ok := x.(json.Number)
	if !ok {
		return nil
	}
	i, err := n.Int64()
	if err != nil {
		return nil
	}
	return r.at(int(i))
}

func (r *reviver) revive(v any) any {
	switch t := v.(type) {
	case []any:
		if len(t) > 0 {
			if tag, ok := t[0].(string); ok {
				switch tag {
				case "Set":
					out := []any{}
					for _, x := range t[1:] {
						out = append(out, r.ref(x))
					}
					return out
				case "Map":
					m := map[string]any{}
					for i := 1; i+1 < len(t); i += 2 {
						m[fmt.Sprint(r.ref(t[i]))] = r.ref(t[i+1])
					}
					return m
				case "Date":
					if len(t) > 1 {
						return t[1]
					}
					return nil
				default: // Reactive, ShallowReactive, Ref, NuxtError, ...
					if len(t) > 1 {
						return r.ref(t[1])
					}
					return nil
				}
			}
		}
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = r.ref(x)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = r.ref(x)
		}
		return out
	default:
		return v
	}
}

type RawQuestion struct {
	ID, Title, Content, Updated, Canonical string
}

func questionsFromPage(page []byte, section string) ([]RawQuestion, error) {
	m := reNuxt.FindSubmatch(page)
	if m == nil {
		return nil, errors.New("на странице нет __NUXT_DATA__ — похоже, изменилась вёрстка сайта")
	}
	dec := json.NewDecoder(bytes.NewReader(m[1]))
	dec.UseNumber()
	var arr []any
	if err := dec.Decode(&arr); err != nil {
		return nil, fmt.Errorf("не удалось разобрать __NUXT_DATA__: %w", err)
	}
	r := &reviver{arr: arr, memo: map[int]any{}, busy: map[int]bool{}}
	root, _ := r.at(0).(map[string]any)
	data, _ := root["data"].(map[string]any)
	if data == nil {
		return nil, errors.New("в __NUXT_DATA__ нет блока data")
	}
	list, ok := data["subtheme-questions_shop.api."+section].([]any)
	if !ok {
		for k, v := range data { // запасной вариант, если ключ немного переименуют
			if strings.HasPrefix(k, "subtheme-questions_") && strings.HasSuffix(k, "."+section) {
				list, ok = v.([]any)
				break
			}
		}
	}
	if !ok {
		keys := make([]string, 0, len(data))
		for k := range data {
			keys = append(keys, k)
		}
		return nil, fmt.Errorf("в данных нет списка вопросов раздела %q (есть ключи: %s)", section, strings.Join(keys, ", "))
	}
	var out []RawQuestion
	for _, x := range list {
		q, ok := x.(map[string]any)
		if !ok {
			continue
		}
		str := func(k string) string {
			if v, ok := q[k]; ok && v != nil {
				return fmt.Sprint(v)
			}
			return ""
		}
		out = append(out, RawQuestion{
			ID: str("id"), Title: str("title"), Content: str("content"),
			Updated: str("updatedDate"), Canonical: str("canonical"),
		})
	}
	return out, nil
}

// ============================================================================
// Разбор HTML ответа: текст, таблицы, примеры (спойлеры)
// ============================================================================

type Example struct {
	Title string `json:"title"`
	Text  string `json:"text"`
}

type Item struct {
	Section  string       `json:"section"`
	QID      string       `json:"qid"`
	Order    int          `json:"order"`
	URL      string       `json:"url"`
	Title    string       `json:"title"`
	Updated  string       `json:"updated"`
	Text     []string     `json:"text"`
	Tables   [][][]string `json:"tables"`
	Examples []Example    `json:"examples"`
}

var (
	reDrop      = regexp.MustCompile(`(?is)<button\b.*?</button>|<span[^>]*spoiler-tooltip[^>]*>.*?</span>|<script\b.*?</script>|<style\b.*?</style>`)
	rePre       = regexp.MustCompile(`(?is)<pre\b[^>]*>(.*?)</pre>`)
	reWS        = regexp.MustCompile(`\s+`)
	reBreak     = regexp.MustCompile(`(?i)<br\s*/?>|</(?:p|div|li|tr|h[1-6]|pre|ul|ol|table|blockquote)\s*>|<(?:p|div|tr|table|ul|ol|pre)\b[^>]*>`)
	reLi        = regexp.MustCompile(`(?i)<li\b[^>]*>`)
	reCellStart = regexp.MustCompile(`(?i)<t[dh]\b[^>]*>`)
	reTag       = regexp.MustCompile(`(?s)<[^>]*>`)

	reTable = regexp.MustCompile(`(?is)<table\b.*?</table>`)
	reRow   = regexp.MustCompile(`(?is)<tr\b.*?</tr>`)
	reCell  = regexp.MustCompile(`(?is)<t[dh]\b[^>]*>(.*?)</t[dh]>`)

	reDivTok       = regexp.MustCompile(`(?i)<div\b|</div\s*>`)
	reSpoilerStart = regexp.MustCompile(`(?i)<div\b[^>]*\bclass="spoiler"[^>]*>`)
	reSpoilerTitle = regexp.MustCompile(`(?i)<div\b[^>]*\bclass="spoiler-title[^"]*"[^>]*>`)
	reSpoilerBody  = regexp.MustCompile(`(?i)<div\b[^>]*\bclass="spoiler-content[^"]*"[^>]*>`)
)

// htmlLines превращает HTML-фрагмент в непустые строки текста.
// Переносы внутри <pre> сохраняются, остальные пробелы схлопываются.
func htmlLines(s string) []string {
	s = reDrop.ReplaceAllString(s, "")
	s = rePre.ReplaceAllStringFunc(s, func(p string) string {
		inner := rePre.FindStringSubmatch(p)[1]
		inner = strings.NewReplacer("\r\n", "\x00", "\n", "\x00", "\r", "\x00", " ", "\x01", "\t", "\x01").Replace(inner)
		return "\x00" + inner + "\x00"
	})
	s = reWS.ReplaceAllString(s, " ")
	s = reLi.ReplaceAllString(s, "\x00• ")
	s = reBreak.ReplaceAllString(s, "\x00")
	s = reCellStart.ReplaceAllString(s, " ")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.NewReplacer("\x00", "\n", "\x01", " ", "\u200b", "").Replace(s)
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.Join(strings.Fields(ln), " "); ln != "" {
			out = append(out, ln)
		}
	}
	return out
}

// matchDiv возвращает позицию сразу за </div>, закрывающим <div> в позиции start.
func matchDiv(s string, start int) int {
	depth := 0
	for _, loc := range reDivTok.FindAllStringIndex(s[start:], -1) {
		if strings.HasPrefix(s[start+loc[0]:start+loc[1]], "</") {
			depth--
			if depth == 0 {
				return start + loc[1]
			}
		} else {
			depth++
		}
	}
	return len(s)
}

func parseTable(t string) [][]string {
	var rows [][]string
	for _, tr := range reRow.FindAllString(t, -1) {
		var cells []string
		for _, m := range reCell.FindAllStringSubmatch(tr, -1) {
			cells = append(cells, strings.Join(htmlLines(m[1]), "\n"))
		}
		if len(cells) > 0 {
			rows = append(rows, cells)
		}
	}
	return rows
}

func parseContent(content string) (text []string, tables [][][]string, examples []Example) {
	var cuts [][2]int
	inside := func(p int) bool {
		for _, c := range cuts {
			if p >= c[0] && p < c[1] {
				return true
			}
		}
		return false
	}
	for _, loc := range reSpoilerStart.FindAllStringIndex(content, -1) {
		if inside(loc[0]) {
			continue
		}
		end := matchDiv(content, loc[0])
		block := content[loc[0]:end]
		var ex Example
		if t := reSpoilerTitle.FindStringIndex(block); t != nil {
			ex.Title = strings.Join(htmlLines(block[t[0]:matchDiv(block, t[0])]), " ")
		}
		if b := reSpoilerBody.FindStringIndex(block); b != nil {
			ex.Text = strings.Join(htmlLines(block[b[0]:matchDiv(block, b[0])]), "\n")
		}
		examples = append(examples, ex)
		cuts = append(cuts, [2]int{loc[0], end})
	}
	for _, loc := range reTable.FindAllStringIndex(content, -1) {
		if inside(loc[0]) {
			continue
		}
		tables = append(tables, parseTable(content[loc[0]:loc[1]]))
		cuts = append(cuts, [2]int{loc[0], loc[1]})
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i][0] < cuts[j][0] })
	var rest strings.Builder
	pos := 0
	for _, c := range cuts {
		if c[0] > pos {
			rest.WriteString(content[pos:c[0]])
		}
		rest.WriteString("<p></p>")
		if c[1] > pos {
			pos = c[1]
		}
	}
	rest.WriteString(content[pos:])
	return htmlLines(rest.String()), tables, examples
}

var reQID = regexp.MustCompile(`/(q\d+)$`)

func toItem(q RawQuestion, section string, order int) *Item {
	qid := "q" + q.ID
	if m := reQID.FindStringSubmatch(q.Canonical); m != nil {
		qid = m[1]
	}
	u := guideBase + "/shop/api/" + section + "/" + qid
	if strings.HasPrefix(q.Canonical, "/") {
		u = guideBase + q.Canonical
	}
	text, tables, examples := parseContent(q.Content)
	return &Item{
		Section: section, QID: qid, Order: order, URL: u,
		Title:   strings.Join(strings.Fields(q.Title), " "),
		Updated: q.Updated, Text: text, Tables: tables, Examples: examples,
	}
}

// ============================================================================
// Разбор примеров запросов
// ============================================================================

type KV struct{ K, V string }

type Request struct {
	Method   string
	Path     string
	Query    []KV
	Headers  []KV
	BodyText string
	Body     any
	BodyOK   bool
}

func (r *Request) header(name string) string {
	norm := func(s string) string { return strings.ReplaceAll(strings.ToLower(s), "-", "") }
	for _, h := range r.Headers {
		if norm(h.K) == norm(name) {
			return h.V
		}
	}
	return ""
}

var (
	reMethodOnly = regexp.MustCompile(`(?i)^(?:request\s+)?(GET|POST|PUT|PATCH|DELETE)$`)
	reMethodLine = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE)\s+(\S.*)$`)
	reURLLine    = regexp.MustCompile(`(?i)^(?:url:\s*)?(https?://.+)$`)
	reHeaderLine = regexp.MustCompile(`^([A-Za-z][\w-]*)\s*:\s*(.*)$`)
	reHostPrefix = regexp.MustCompile(`^https?://[^/]+`)
	reHTTPSuffix = regexp.MustCompile(`\s+HTTP/\S+$`)
)

// parseRequest понимает все встреченные в документации формы записи:
// «POST /api/v2/orders», «GET» + URL отдельной строкой, «request PUT» + «url: ...»,
// «GET /shop/api/... HTTP/1.1», перенос query-строки на несколько строк.
func parseRequest(text string) (*Request, bool) {
	r := &Request{}
	var methodPath, fullURL string
	var body []string
	inBody := false
	for _, raw := range strings.Split(text, "\n") {
		s := strings.TrimSpace(raw)
		if inBody {
			body = append(body, raw)
			continue
		}
		if s == "" || strings.HasPrefix(s, "//") {
			continue
		}
		if m := reMethodOnly.FindStringSubmatch(s); m != nil {
			if r.Method == "" {
				r.Method = strings.ToUpper(m[1])
			}
			continue
		}
		if m := reMethodLine.FindStringSubmatch(s); m != nil {
			if r.Method == "" {
				r.Method = m[1]
			}
			rest := reHTTPSuffix.ReplaceAllString(strings.TrimSpace(m[2]), "")
			if strings.HasPrefix(rest, "http") {
				if fullURL == "" {
					fullURL = rest
				}
			} else if methodPath == "" {
				methodPath = rest
			}
			continue
		}
		if m := reURLLine.FindStringSubmatch(s); m != nil {
			if fullURL == "" {
				fullURL = m[1]
			}
			continue
		}
		if strings.HasPrefix(s, "&") && fullURL != "" {
			fullURL += s
			continue
		}
		if strings.HasPrefix(strings.ToUpper(s), "HTTP/") {
			continue
		}
		if s[0] == '{' || s[0] == '[' {
			inBody = true
			body = append(body, raw)
			continue
		}
		if m := reHeaderLine.FindStringSubmatch(s); m != nil {
			r.Headers = append(r.Headers, KV{m[1], strings.TrimSpace(m[2])})
		}
	}
	if r.Method == "" && methodPath == "" && fullURL == "" {
		return nil, false
	}
	// URL надёжнее строки метода: в части примеров она скопирована с чужого запроса
	p := methodPath
	if fullURL != "" {
		p = reHostPrefix.ReplaceAllString(fullURL, "")
	}
	q := ""
	if i := strings.Index(p, "?"); i >= 0 {
		p, q = p[:i], p[i+1:]
	}
	p = strings.ReplaceAll(p, " ", "")
	if strings.HasPrefix(p, "/shop/") {
		p = strings.TrimPrefix(p, "/shop")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	r.Path = p
	for _, part := range strings.Split(q, "&") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" || strings.EqualFold(k, "host") || strings.HasPrefix(strings.ToUpper(k), "HTTP/") {
			continue // мусор вида «?HTTP/1.1=&Host= kaspi.kz» в примерах
		}
		r.Query = append(r.Query, KV{k, v})
	}
	if r.Method == "" {
		r.Method = "GET"
		if len(body) > 0 {
			r.Method = "POST"
		}
	}
	r.BodyText = strings.TrimSpace(strings.Join(body, "\n"))
	if r.BodyText != "" {
		r.Body, r.BodyOK = lenientJSON(r.BodyText)
	}
	return r, true
}

// ============================================================================
// «Прощающий» JSON: ёлочки вместо кавычек, пропущенные и висячие запятые,
// лишние и недостающие скобки, переносы строк внутри строк
// ============================================================================

var (
	reTrailingComma = regexp.MustCompile(`,\s*([}\]])`)
	reMissingComma  = regexp.MustCompile(`("|\d|true|false|null|[}\]])[ \t]*\n(\s*")`)
	quoteFixer      = strings.NewReplacer("«", `"`, "»", `"`, "“", `"`, "”", `"`, "\u00a0", " ")
)

func lenientJSON(s string) (any, bool) {
	if v, err := decodeOrdered(s); err == nil {
		return v, true
	}
	t := quoteFixer.Replace(s)
	t = reMissingComma.ReplaceAllString(t, "$1,\n$2")
	t = reTrailingComma.ReplaceAllString(t, "$1")
	var b strings.Builder
	var stack []byte
	inStr, esc := false, false
	for i := 0; i < len(t); i++ {
		ch := t[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case ch == '\\':
				esc = true
			case ch == '"':
				inStr = false
			case ch == '\n' || ch == '\r':
				continue // разорванная переносом строка
			}
			b.WriteByte(ch)
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) == 0 || stack[len(stack)-1] != ch {
				continue // лишняя закрывающая скобка
			}
			stack = stack[:len(stack)-1]
		}
		b.WriteByte(ch)
	}
	for i := len(stack) - 1; i >= 0; i-- {
		b.WriteByte(stack[i])
	}
	v, err := decodeOrdered(b.String())
	return v, err == nil
}

func decodeOrdered(s string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	v, err := readValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("лишние данные после JSON")
	}
	return v, nil
}

func readValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch d {
	case '{':
		m := NewM()
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k, _ := kt.(string)
			v, err := readValue(dec)
			if err != nil {
				return nil, err
			}
			m.Set(k, v)
		}
		_, err := dec.Token()
		return m, err
	case '[':
		arr := []any{}
		for dec.More() {
			v, err := readValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		_, err := dec.Token()
		return arr, err
	}
	return nil, fmt.Errorf("неожиданный токен %v", d)
}

// ============================================================================
// Подсказки из таблиц «Параметр/Атрибут — Значение»
// ============================================================================

type hint struct {
	desc   string
	values []string
}

var (
	reIdent = regexp.MustCompile(`^[A-Za-z_][\w.\[\]$-]*=*$`)
	reUpper = regexp.MustCompile(`[A-Z][A-Z0-9_]{2,}`)
)

func tableHints(tables [][][]string) map[string]*hint {
	h := map[string]*hint{}
	for _, tb := range tables {
		var last *hint
		for ri, row := range tb {
			if ri == 0 || len(row) == 0 {
				continue
			}
			name := strings.TrimSpace(row[0])
			var rest []string
			for _, c := range row[1:] {
				if c = strings.TrimSpace(c); c != "" {
					rest = append(rest, c)
				}
			}
			switch {
			case name != "" && reIdent.MatchString(name):
				last = &hint{desc: strings.ReplaceAll(strings.Join(rest, " "), "\n", " ")}
				h[strings.ToLower(strings.TrimRight(name, "="))] = last
			case name == "" && last != nil && len(rest) > 0:
				vals, d := rest, ""
				if len(rest) > 1 {
					vals, d = rest[:len(rest)-1], rest[len(rest)-1]
				}
				for _, v := range reUpper.FindAllString(strings.Join(vals, " "), -1) {
					if !contains(last.values, v) {
						last.values = append(last.values, v)
					}
				}
				if d != "" {
					last.desc += " (" + strings.ReplaceAll(d, "\n", "; ") + ")"
				}
			}
		}
	}
	return h
}

var reLastBracket = regexp.MustCompile(`\[[^\[\]]*\]$`)

func lookupHint(h map[string]*hint, names ...string) *hint {
	for _, n := range names {
		k := strings.ToLower(strings.TrimRight(n, "="))
		for k != "" {
			if v := h[k]; v != nil {
				return v
			}
			nk := reLastBracket.ReplaceAllString(k, "") // filter[a][b][$ge] → filter[a][b]
			if nk == k {
				break
			}
			k = nk
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ============================================================================
// Сборка OpenAPI
// ============================================================================

func inferSchema(v any, hints map[string]*hint, name string) *OMap {
	s := NewM()
	switch t := v.(type) {
	case *OMap:
		s.Set("type", "object")
		props := NewM()
		for _, k := range t.keys {
			props.Set(k, inferSchema(t.vals[k], hints, k))
		}
		s.Set("properties", props)
	case []any:
		s.Set("type", "array")
		if len(t) > 0 {
			s.Set("items", inferSchema(t[0], hints, ""))
		} else {
			s.Set("items", NewM())
		}
	case string:
		s.Set("type", "string")
	case json.Number:
		if strings.ContainsAny(string(t), ".eE") {
			s.Set("type", "number")
		} else {
			s.Set("type", "integer")
		}
	case bool:
		s.Set("type", "boolean")
	case nil:
		s.Set("nullable", true)
	}
	if name != "" {
		if h := hints[strings.ToLower(name)]; h != nil {
			desc := h.desc
			if len(h.values) > 0 {
				desc = strings.TrimSpace(desc + " Значения: " + strings.Join(h.values, ", "))
				if sv, ok := v.(string); ok && contains(h.values, sv) {
					enum := make([]any, len(h.values))
					for i, x := range h.values {
						enum[i] = x
					}
					s.Set("enum", enum)
				}
			}
			if desc != "" {
				s.Set("description", desc)
			}
		}
	}
	return s
}

var (
	reDigit   = regexp.MustCompile(`\d`)
	reVersion = regexp.MustCompile(`^v\d+$`)
	reNonWord = regexp.MustCompile(`\W+`)
	reB64     = regexp.MustCompile(`^[A-Za-z0-9_=-]{6,}$`)
)

// looksLikeID ловит base64-идентификаторы вроде MDAwOTIwMDA: в них много
// заглавных букв, в отличие от имён ресурсов (deliveryPointOfService).
func looksLikeID(s string) bool {
	if !reB64.MatchString(s) {
		return false
	}
	upper := 0
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			upper++
		}
	}
	return float64(upper)/float64(len(s)) >= 0.4
}

// templatePath заменяет заглушки в пути (ordersID, orderentriesId=, {id},
// MDAwOTIwMDA) на {параметр}. Возвращает шаблон и пары (имя, исходный сегмент).
func templatePath(p string) (string, []KV) {
	segs := strings.Split(p, "/")
	var params []KV
	used := map[string]int{}
	for i, seg := range segs {
		if seg == "" {
			continue
		}
		clean := strings.Trim(seg, "{}<>:=")
		isParam := strings.HasPrefix(seg, "{") || strings.HasPrefix(seg, "<") ||
			strings.HasSuffix(clean, "ID") || strings.HasSuffix(clean, "Id") ||
			strings.HasSuffix(seg, "=") || looksLikeID(clean) ||
			(reDigit.MatchString(seg) && !reVersion.MatchString(seg))
		if !isParam {
			continue
		}
		name := clean
		switch {
		case strings.HasSuffix(name, "ID"):
			name = strings.TrimSuffix(name, "ID") + "Id"
		case reDigit.MatchString(name) || looksLikeID(name) || name == "":
			name = "id"
		}
		name = reNonWord.ReplaceAllString(name, "")
		if used[name]++; used[name] > 1 {
			name = fmt.Sprintf("%s%d", name, used[name])
		}
		params = append(params, KV{name, clean})
		segs[i] = "{" + name + "}"
	}
	return strings.Join(segs, "/"), params
}

func tableMarkdown(tb [][]string) string {
	if len(tb) == 0 {
		return ""
	}
	w := 0
	for _, r := range tb {
		if len(r) > w {
			w = len(r)
		}
	}
	cell := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, "|", `\|`), "\n", "<br>")
	}
	var b strings.Builder
	for ri, r := range tb {
		b.WriteString("|")
		for i := 0; i < w; i++ {
			v := ""
			if i < len(r) {
				v = cell(r[i])
			}
			b.WriteString(" " + v + " |")
		}
		b.WriteString("\n")
		if ri == 0 {
			b.WriteString("|" + strings.Repeat("---|", w) + "\n")
		}
	}
	return b.String()
}

var reRespHead = regexp.MustCompile(`(?i)^(пример\s+)?ответ(\s.*)?$`)

// splitSegments делит спойлер вида «запрос … Ответ … Ответ в случае ошибки …» на части.
func splitSegments(ex Example) []Example {
	var segs []Example
	title, cur := ex.Title, []string{}
	for _, ln := range strings.Split(ex.Text, "\n") {
		if len([]rune(ln)) < 50 && reRespHead.MatchString(ln) && len(cur) > 0 {
			segs = append(segs, Example{title, strings.Join(cur, "\n")})
			title, cur = ln, nil
			continue
		}
		cur = append(cur, ln)
	}
	if len(cur) > 0 {
		segs = append(segs, Example{title, strings.Join(cur, "\n")})
	}
	return segs
}

type opState struct {
	method, path string
	section      string
	opID         string
	titles       []string
	docs         []string
	guide        []any
	params       []any
	seen         map[string]bool
	reqContent   *OMap
	respContent  *OMap
}

func mediaExample(content *OMap, media, key, title, raw string, parsed any, ok bool, hints map[string]*hint) {
	isJSON := strings.Contains(media, "json")
	var mt *OMap
	if v, exists := content.Get(media); exists {
		mt = v.(*OMap)
	} else {
		mt = NewM()
		content.Set(media, mt)
	}
	var exs *OMap
	if v, exists := mt.Get("examples"); exists {
		exs = v.(*OMap)
	} else {
		exs = NewM()
	}
	if _, has := mt.Get("schema"); !has {
		if ok && isJSON {
			mt.Set("schema", inferSchema(parsed, hints, ""))
		} else {
			mt.Set("schema", NewM().Set("type", "string"))
		}
	}
	ex := NewM().Set("summary", title)
	switch {
	case ok && isJSON:
		ex.Set("value", parsed)
	case ok: // JSON в text/plain (импорт товаров) — отдаём исправленный валидный JSON
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		enc.Encode(parsed)
		ex.Set("value", strings.TrimSpace(b.String()))
	default:
		ex.Set("value", raw)
	}
	exs.Set(key, ex)
	mt.Set("examples", exs)
}

// Два заголовка, без которых не проходит ни один запрос. X-Auth-Token в
// примерах Гида есть, X-Merchant-Uid не описан вовсе — в одном кабинете может
// быть несколько магазинов, и сервер должен понимать, про какой спрашивают.
// Оба объявляем обычными параметрами с in: header, чтобы они были видны в
// каждой операции и доезжали до клиентов вроде Bruno.
const (
	authTokenHeader   = "X-Auth-Token"
	merchantUIDHeader = "X-Merchant-Uid"
)

const (
	authTokenDesc   = "Токен из кабинета продавца: Настройки → Токен API."
	merchantUIDDesc = "Идентификатор магазина. В Kaspi Гид не описан, но обязателен: " +
		"в одном кабинете может быть несколько магазинов."
)

// Значения-заглушки в формате переменных Bruno: заведите их в окружении, и
// заголовки подставятся сами. Настоящий токен в схему не попадает никогда —
// она выкладывается на хостинг в открытый доступ.
const (
	authTokenExample   = "{{token}}"
	merchantUIDExample = "{{merchantUid}}"
)

// withRequiredHeaders ставит оба заголовка в начало списка параметров операции.
// Если заголовок уже пришёл из примеров документации, не дублируем его —
// только помечаем обязательным.
func withRequiredHeaders(params []any, merchantUID string) []any {
	if merchantUID == "" {
		// Пустое значение хуже отсутствующего: клиент отправит пустой
		// заголовок, и тот перекроет всё, что задано на уровне коллекции.
		merchantUID = merchantUIDExample
	}
	params = ensureHeader(params, merchantUIDHeader, merchantUIDDesc, merchantUID)
	return ensureHeader(params, authTokenHeader, authTokenDesc, authTokenExample)
}

func ensureHeader(params []any, name, desc, example string) []any {
	for _, p := range params {
		m, ok := p.(*OMap)
		if !ok {
			continue
		}
		n, _ := m.Get("name")
		in, _ := m.Get("in")
		if s, ok := n.(string); ok && in == "header" && strings.EqualFold(s, name) {
			m.Set("required", true)
			return params
		}
	}
	p := NewM().Set("name", name).Set("in", "header").Set("required", true).
		Set("description", desc).
		Set("schema", NewM().Set("type", "string"))
	if example != "" {
		p.Set("example", example)
	}
	return append([]any{p}, params...)
}

func buildSpec(items []*Item, merchantUID string) *OMap {
	ops := map[string]*opState{}
	var order []string
	var undocumented []*Item
	latest := ""

	for _, it := range items {
		if d, err := time.Parse("02.01.2006", it.Updated); err == nil {
			if iso := d.Format("2006-01-02"); iso > latest {
				latest = iso
			}
		}
		hints := tableHints(it.Tables)

		type reqEx struct {
			r     *Request
			title string
		}
		var reqs []reqEx
		var resps []Example
		for _, ex := range it.Examples {
			for _, seg := range splitSegments(ex) {
				tl := strings.ToLower(seg.Title)
				isReq, isResp := strings.Contains(tl, "запрос"), strings.Contains(tl, "ответ")
				if isReq || !isResp {
					if r, ok := parseRequest(seg.Text); ok {
						reqs = append(reqs, reqEx{r, seg.Title})
						continue
					}
				}
				resps = append(resps, seg)
			}
		}
		if len(reqs) == 0 {
			undocumented = append(undocumented, it)
			continue
		}

		var doc strings.Builder
		fmt.Fprintf(&doc, "**[%s](%s)**  \n_Обновлено: %s_\n\n", it.Title, it.URL, it.Updated)
		text := it.Text
		if len(text) > 40 {
			text = text[:40]
		}
		doc.WriteString(strings.Join(text, "\n\n"))
		for _, tb := range it.Tables {
			doc.WriteString("\n\n" + tableMarkdown(tb))
		}

		var firstOp *opState
		for ri, rq := range reqs {
			r := rq.r
			tpl, pparams := templatePath(r.Path)
			key := r.Method + " " + tpl
			st := ops[key]
			if st == nil {
				st = &opState{method: strings.ToLower(r.Method), path: tpl, section: it.Section,
					opID: it.Section + "_" + it.QID, seen: map[string]bool{},
					reqContent: NewM(), respContent: NewM()}
				ops[key] = st
				order = append(order, key)
			}
			if firstOp == nil {
				firstOp = st
			}
			if !st.seen["q:"+it.QID] {
				st.seen["q:"+it.QID] = true
				st.titles = append(st.titles, it.Title)
				st.docs = append(st.docs, doc.String())
				st.guide = append(st.guide, NewM().Set("id", it.QID).Set("title", it.Title).
					Set("url", it.URL).Set("updated", it.Updated))
			}
			addParam := func(in, name, example, hintName string) {
				k := in + ":" + name
				if st.seen[k] {
					return
				}
				st.seen[k] = true
				p := NewM().Set("name", name).Set("in", in).Set("required", in == "path").
					Set("schema", NewM().Set("type", "string"))
				if h := lookupHint(hints, hintName, name); h != nil {
					desc := h.desc
					if len(h.values) > 0 {
						desc = strings.TrimSpace(desc + " Значения: " + strings.Join(h.values, ", "))
						if contains(h.values, example) {
							enum := []any{}
							for _, v := range h.values {
								enum = append(enum, v)
							}
							p.Set("schema", NewM().Set("type", "string").Set("enum", enum))
						}
					}
					if desc != "" {
						p.Set("description", desc)
					}
				}
				if example != "" && in != "path" {
					p.Set("example", example)
				}
				st.params = append(st.params, p)
			}
			for _, pp := range pparams {
				addParam("path", pp.K, "", pp.V)
			}
			for _, q := range r.Query {
				addParam("query", q.K, q.V, q.K)
			}
			for _, h := range r.Headers {
				switch strings.ReplaceAll(strings.ToLower(h.K), "-", "") {
				case "host", "contenttype", "xauthtoken", "accept", "contentlength":
					continue
				}
				addParam("header", h.K, h.V, h.K)
			}
			if r.BodyText != "" {
				media := r.header("Content-Type")
				if media == "" && st.reqContent.Len() > 0 {
					media = st.reqContent.keys[0] // как у других примеров этой операции
				}
				if media == "" && strings.Contains(tpl, "/v2/") {
					media = "application/vnd.api+json"
				}
				if media == "" {
					media = "application/json"
				}
				media = strings.TrimSpace(strings.Split(media, ";")[0])
				exKey := it.QID
				if ri > 0 {
					exKey = fmt.Sprintf("%s_%d", it.QID, ri+1)
				}
				mediaExample(st.reqContent, media, exKey, rq.title+" — "+it.Title, r.BodyText, r.Body, r.BodyOK, hints)
			}
		}

		respMedia := "application/vnd.api+json"
		if a := reqs[0].r.header("Accept"); strings.Contains(a, "json") {
			respMedia = strings.TrimSpace(strings.Split(a, ";")[0])
		}
		for i, rs := range resps {
			parsed, ok := lenientJSON(rs.Text)
			media := respMedia
			if !ok {
				media = "text/plain"
			}
			exKey := it.QID
			if i > 0 {
				exKey = fmt.Sprintf("%s_%d", it.QID, i+1)
			}
			title := it.Title
			if rs.Title != "" && !strings.Contains(strings.ToLower(rs.Title), "посмотреть") {
				title = rs.Title + " — " + it.Title
			}
			mediaExample(firstOp.respContent, media, exKey, title, rs.Text, parsed, ok, hints)
		}
	}

	paths := map[string]*OMap{}
	for _, key := range order {
		st := ops[key]
		summary := st.titles[0]
		if len(st.titles) > 1 {
			summary = fmt.Sprintf("%s (+%d)", st.titles[0], len(st.titles)-1)
		}
		op := NewM().
			Set("tags", []any{st.section}).
			Set("summary", summary).
			Set("operationId", st.opID).
			Set("description", strings.Join(st.docs, "\n\n---\n\n"))
		op.Set("parameters", withRequiredHeaders(st.params, merchantUID))
		if st.reqContent.Len() > 0 {
			op.Set("requestBody", NewM().Set("required", true).Set("content", st.reqContent))
		}
		resp200 := NewM().Set("description", "Успешный ответ")
		if st.respContent.Len() > 0 {
			resp200.Set("content", st.respContent)
		}
		op.Set("responses", NewM().Set("200", resp200))
		op.Set("x-kaspi-guide", st.guide)
		if paths[st.path] == nil {
			paths[st.path] = NewM()
		}
		paths[st.path].Set(st.method, op)
	}
	var pathKeys []string
	for k := range paths {
		pathKeys = append(pathKeys, k)
	}
	sort.Strings(pathKeys)
	pathsM := NewM()
	for _, k := range pathKeys {
		pathsM.Set(k, paths[k])
	}

	desc := "Собрано автоматически из Kaspi Гид (раздел «API»). Схемы выведены из примеров " +
		"документации — это черновик, сверяйтесь с первоисточником по ссылкам в описании операций."
	if len(undocumented) > 0 {
		desc += "\n\nВопросы без примера запроса (в paths не попали):"
		for _, it := range undocumented {
			desc += fmt.Sprintf("\n- [%s](%s)", it.Title, it.URL)
		}
	}
	if latest == "" {
		latest = "unknown"
	}
	var tags []any
	seenTag := map[string]bool{}
	for _, it := range items {
		if !seenTag[it.Section] {
			seenTag[it.Section] = true
			t := sectionTitles[it.Section]
			if t == "" {
				t = it.Section
			}
			tags = append(tags, NewM().Set("name", it.Section).Set("description", t))
		}
	}
	return NewM().
		Set("openapi", "3.0.3").
		Set("info", NewM().Set("title", "Kaspi Shop API (из Kaspi Гид)").Set("version", latest).Set("description", desc)).
		Set("servers", []any{NewM().Set("url", apiServer)}).
		Set("tags", tags).
		Set("paths", pathsM)
}

// ============================================================================
// Сравнение снимков и дифф
// ============================================================================

func renderItem(it *Item) []string {
	lines := []string{"# " + it.Title, it.URL, "Обновлено: " + it.Updated, ""}
	lines = append(lines, it.Text...)
	for _, tb := range it.Tables {
		lines = append(lines, "")
		for _, r := range tb {
			cells := make([]string, len(r))
			for i, c := range r {
				cells[i] = strings.ReplaceAll(c, "\n", " / ")
			}
			lines = append(lines, "| "+strings.Join(cells, " | ")+" |")
		}
	}
	for _, ex := range it.Examples {
		lines = append(lines, "", "--- "+ex.Title+" ---")
		lines = append(lines, strings.Split(ex.Text, "\n")...)
	}
	return lines
}

func unifiedDiff(a, b []string, ctx int, nameA, nameB string) string {
	n, m := len(a), len(b)
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", nameA, nameB)
	if n*m > 4_000_000 { // слишком большие тексты — без выравнивания
		for _, l := range a {
			out.WriteString("-" + l + "\n")
		}
		for _, l := range b {
			out.WriteString("+" + l + "\n")
		}
		return out.String()
	}
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	type op struct {
		kind   byte
		line   string
		ai, bi int
	}
	var ops []op
	for i, j := 0, 0; i < n || j < m; {
		switch {
		case i < n && j < m && a[i] == b[j]:
			ops = append(ops, op{' ', a[i], i, j})
			i++
			j++
		case i < n && (j == m || dp[i+1][j] >= dp[i][j+1]):
			ops = append(ops, op{'-', a[i], i, j})
			i++
		default:
			ops = append(ops, op{'+', b[j], i, j})
			j++
		}
	}
	for k := 0; k < len(ops); {
		if ops[k].kind == ' ' {
			k++
			continue
		}
		start := k - ctx
		if start < 0 {
			start = 0
		}
		end := k
		for {
			for end < len(ops) && ops[end].kind != ' ' {
				end++
			}
			e := end
			for e < len(ops) && ops[e].kind == ' ' {
				e++
			}
			if e < len(ops) && e-end <= 2*ctx {
				end = e
				continue
			}
			end += ctx
			if end > len(ops) {
				end = len(ops)
			}
			break
		}
		aLen, bLen := 0, 0
		for _, o := range ops[start:end] {
			if o.kind != '+' {
				aLen++
			}
			if o.kind != '-' {
				bLen++
			}
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", ops[start].ai+1, aLen, ops[start].bi+1, bLen)
		for _, o := range ops[start:end] {
			out.WriteString(string(o.kind) + o.line + "\n")
		}
		k = end
	}
	return out.String()
}

func compare(old, cur map[string]*Item) (added, removed, changed []string, diff string) {
	var parts []string
	keys := func(m map[string]*Item) []string {
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return ks
	}
	for _, k := range keys(cur) {
		o, ok := old[k]
		if !ok {
			added = append(added, k)
			continue
		}
		a, b := renderItem(o), renderItem(cur[k])
		if strings.Join(a, "\n") != strings.Join(b, "\n") {
			changed = append(changed, k)
			parts = append(parts, unifiedDiff(a, b, 2, k+" (было)", k+" (стало)"))
		}
	}
	for _, k := range added {
		lines := renderItem(cur[k])
		parts = append(parts, "+++ НОВЫЙ ВОПРОС "+k+"\n+"+strings.Join(lines, "\n+"))
	}
	for _, k := range keys(old) {
		if _, ok := cur[k]; !ok {
			removed = append(removed, k)
			parts = append(parts, "--- УДАЛЁН ВОПРОС "+k+"\n-"+strings.Join(renderItem(old[k]), "\n-"))
		}
	}
	return added, removed, changed, strings.Join(parts, "\n\n")
}

// ============================================================================
// Уведомления
// ============================================================================

type Notifier struct{ cfg *Config }

func (n *Notifier) Send(lines []string, files ...string) {
	plain := html.UnescapeString(reTag.ReplaceAllString(strings.Join(lines, "\n"), ""))
	if n.cfg.DryRun || (n.cfg.TgToken == "" && n.cfg.Webhook == "") {
		fmt.Println("\n===== УВЕДОМЛЕНИЕ =====\n" + plain + "\n=======================")
		for _, f := range files {
			fmt.Println("[файл]", f)
		}
		return
	}
	if n.cfg.TgToken != "" && n.cfg.TgChat != "" {
		n.telegram(lines, files)
	}
	if n.cfg.Webhook != "" {
		if len([]rune(plain)) > 1900 {
			plain = string([]rune(plain)[:1900])
		}
		body, _ := json.Marshal(map[string]string{"text": plain, "content": plain})
		resp, err := httpClient.Post(n.cfg.Webhook, "application/json", bytes.NewReader(body))
		if err != nil {
			log.Printf("Webhook не отправлен: %v", err)
		} else {
			resp.Body.Close()
		}
	}
}

func (n *Notifier) telegram(lines []string, files []string) {
	api := "https://api.telegram.org/bot" + n.cfg.TgToken
	var chunks []string
	cur := ""
	for _, l := range lines { // режем по строкам, чтобы не порвать HTML-теги
		if len(cur)+len(l)+1 > 3900 && cur != "" {
			chunks = append(chunks, cur)
			cur = ""
		}
		cur += l + "\n"
	}
	if cur != "" {
		chunks = append(chunks, cur)
	}
	for _, ch := range chunks {
		resp, err := httpClient.PostForm(api+"/sendMessage", url.Values{
			"chat_id": {n.cfg.TgChat}, "text": {ch}, "parse_mode": {"HTML"},
			"disable_web_page_preview": {"true"},
		})
		if err != nil {
			log.Printf("Telegram: сообщение не отправлено: %v", err)
			continue
		}
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			log.Printf("Telegram: HTTP %d: %s", resp.StatusCode, b)
		}
		resp.Body.Close()
	}
	for _, f := range files {
		if err := n.tgDocument(api, f); err != nil {
			log.Printf("Telegram: файл %s не отправлен: %v", filepath.Base(f), err)
		}
	}
}

func (n *Notifier) tgDocument(api, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("chat_id", n.cfg.TgChat)
	fw, err := w.CreateFormFile("document", filepath.Base(path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return err
	}
	w.Close()
	resp, err := httpClient.Post(api+"/sendDocument", w.FormDataContentType(), &buf)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	return nil
}

// swaggerLink — ссылка на страницу со Swagger на хостинге, если она задана.
func swaggerLink(cfg *Config) []string {
	if cfg.PublicURL == "" {
		return nil
	}
	u := html.EscapeString(cfg.PublicURL)
	return []string{"", fmt.Sprintf(`📘 Swagger на сайте: <a href="%s">%s</a>`, u, u)}
}

func link(it *Item) string {
	t := []rune(it.Title)
	if len(t) > 120 {
		t = t[:120]
	}
	return fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(it.URL), html.EscapeString(string(t)))
}

// ============================================================================
// Основной прогон
// ============================================================================

func scrape(cfg *Config) ([]*Item, error) {
	var items []*Item
	for si, sec := range cfg.Sections {
		var page []byte
		var err error
		if cfg.From != "" {
			page, err = os.ReadFile(filepath.Join(cfg.From, sec+".html"))
		} else {
			page, err = fetch(guideBase + "/shop/api/" + sec)
		}
		if err != nil {
			return nil, err
		}
		qs, err := questionsFromPage(page, sec)
		if err != nil {
			dbg := filepath.Join(cfg.Dir, "debug")
			os.MkdirAll(dbg, 0o755)
			os.WriteFile(filepath.Join(dbg, sec+".html"), page, 0o644)
			return nil, fmt.Errorf("раздел %s: %w (страница сохранена в %s)", sec, err, dbg)
		}
		for i, q := range qs {
			items = append(items, toItem(q, sec, si*1000+i))
		}
		log.Printf("Раздел %s: %d вопросов", sec, len(qs))
		if cfg.From == "" && si < len(cfg.Sections)-1 {
			time.Sleep(time.Second)
		}
	}
	return items, nil
}

func runOnce(cfg *Config) int {
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		log.Printf("Не могу создать %s: %v", cfg.Dir, err)
		return 2
	}
	notifier := &Notifier{cfg}
	snapPath := filepath.Join(cfg.Dir, "snapshot.json")
	specPath := filepath.Join(cfg.Dir, "openapi.json")

	items, err := scrape(cfg)
	if err != nil {
		log.Printf("Ошибка при сборе: %v", err)
		notifier.Send([]string{"⚠️ <b>Kaspi API watcher</b>: ошибка при обходе сайта", html.EscapeString(err.Error())})
		return 2
	}
	cur := map[string]*Item{}
	for _, it := range items {
		cur[it.Section+"/"+it.QID] = it
	}

	old := map[string]*Item{}
	if b, err := os.ReadFile(snapPath); err == nil {
		if err := json.Unmarshal(b, &old); err != nil {
			log.Printf("snapshot.json повреждён (%v), начинаю заново", err)
			old = map[string]*Item{}
		}
	}

	// Защита от ложных «всё удалили», если сайт поменял вёрстку
	if len(cur) == 0 || (len(old) > 0 && len(cur)*2 < len(old)) {
		log.Printf("Подозрительно мало вопросов: %d (было %d). Снимок не обновляю.", len(cur), len(old))
		notifier.Send([]string{fmt.Sprintf("⚠️ <b>Kaspi API watcher</b>: собрано подозрительно мало вопросов (%d, было %d).", len(cur), len(old)),
			"Скорее всего, изменилась вёрстка сайта."})
		return 3
	}

	spec := buildSpec(items, cfg.MerchantUID)
	added, removed, changed, diff := compare(old, cur)
	hasChanges := len(added)+len(removed)+len(changed) > 0

	var diffFile string
	if len(old) > 0 && hasChanges {
		hist := filepath.Join(cfg.Dir, "history", time.Now().Format("2006-01-02_15-04-05"))
		os.MkdirAll(hist, 0o755)
		diffFile = filepath.Join(hist, "diff.txt")
		os.WriteFile(diffFile, []byte(diff), 0o644)
		for _, f := range []string{specPath, snapPath} {
			if b, err := os.ReadFile(f); err == nil {
				os.WriteFile(filepath.Join(hist, "prev_"+filepath.Base(f)), b, 0o644)
			}
		}
	}
	if err := writeJSON(specPath, spec); err != nil {
		log.Printf("Не удалось записать %s: %v", specPath, err)
		return 2
	}
	if err := writeJSON(snapPath, cur); err != nil {
		log.Printf("Не удалось записать %s: %v", snapPath, err)
		return 2
	}

	publish(cfg, spec, specPath)

	nOps := 0
	if p, ok := spec.Get("paths"); ok {
		for _, k := range p.(*OMap).keys {
			v, _ := p.(*OMap).Get(k)
			nOps += v.(*OMap).Len()
		}
	}

	if len(old) == 0 {
		log.Printf("Первый снимок: %d вопросов, %d операций в Swagger.", len(cur), nOps)
		lines := []string{"✅ <b>Kaspi API watcher</b> запущен.",
			fmt.Sprintf("Собрано вопросов: %d, операций в Swagger: %d.", len(cur), nOps)}
		lines = append(lines, swaggerLink(cfg)...)
		lines = append(lines, "Дальше буду писать только при изменениях.")
		notifier.Send(lines, specPath)
		return 0
	}
	if !hasChanges {
		log.Printf("Изменений нет (%d вопросов).", len(cur))
		return 0
	}

	lines := []string{"🔔 <b>Изменения в документации Kaspi API</b>", ""}
	if len(added) > 0 {
		lines = append(lines, fmt.Sprintf("<b>Новые вопросы (%d):</b>", len(added)))
		for _, k := range added {
			lines = append(lines, "➕ "+link(cur[k]))
		}
		lines = append(lines, "")
	}
	if len(changed) > 0 {
		lines = append(lines, fmt.Sprintf("<b>Изменены (%d):</b>", len(changed)))
		for _, k := range changed {
			l := "✏️ " + link(cur[k])
			if cur[k].Updated != old[k].Updated {
				l += fmt.Sprintf(" (обновлено %s)", html.EscapeString(cur[k].Updated))
			}
			lines = append(lines, l)
		}
		lines = append(lines, "")
	}
	if len(removed) > 0 {
		lines = append(lines, fmt.Sprintf("<b>Удалены (%d):</b>", len(removed)))
		for _, k := range removed {
			lines = append(lines, "➖ "+link(old[k]))
		}
		lines = append(lines, "")
	}
	short := diff
	if r := []rune(short); len(r) > 2500 {
		short = string(r[:2500]) + "\n…(полный дифф в файле)"
	}
	lines = append(lines, "<pre>"+html.EscapeString(short)+"</pre>")
	lines = append(lines, swaggerLink(cfg)...)
	notifier.Send(lines, diffFile, specPath)
	log.Printf("Изменения: +%d ~%d -%d. Уведомление отправлено.", len(added), len(changed), len(removed))
	if cfg.ExitCode {
		return 1
	}
	return 0
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime)
	cfg := loadConfig()
	if cfg.Interval <= 0 {
		os.Exit(runOnce(cfg))
	}
	for {
		runOnce(cfg)
		log.Printf("Следующая проверка через %d мин.", cfg.Interval)
		time.Sleep(time.Duration(cfg.Interval) * time.Minute)
	}
}
