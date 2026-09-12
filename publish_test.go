package main

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Игрушечный FTP-сервер: ровно те команды, которыми пользуется publish.go.
type fakeFTP struct {
	t       *testing.T
	addr    string
	noEPSV  bool        // сервер не умеет EPSV — клиент должен откатиться на PASV
	tlsCfg  *tls.Config // не nil — сервер требует AUTH TLS
	mu      sync.Mutex
	files   map[string][]byte // путь -> содержимое
	dirs    map[string]bool
	made    []string // какие каталоги пришлось создавать
	stopped chan struct{}
}

func startFakeFTP(t *testing.T, noEPSV bool) *fakeFTP {
	return startFakeFTPTLS(t, noEPSV, nil)
}

func startFakeFTPTLS(t *testing.T, noEPSV bool, tlsCfg *tls.Config) *fakeFTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeFTP{t: t, addr: ln.Addr().String(), noEPSV: noEPSV, tlsCfg: tlsCfg,
		files: map[string][]byte{}, dirs: map[string]bool{"/": true}, stopped: make(chan struct{})}
	go func() {
		defer close(s.stopped)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *fakeFTP) serve(c net.Conn) {
	defer c.Close()
	conn := c // после AUTH TLS здесь окажется TLS-обёртка
	r := bufio.NewReader(conn)
	say := func(format string, a ...any) { fmt.Fprintf(conn, format+"\r\n", a...) }

	authed := s.tlsCfg == nil // без TLS логин разрешаем сразу
	cwd := "/"
	var dataLn net.Listener
	defer func() {
		if dataLn != nil {
			dataLn.Close()
		}
	}()
	var pending string // имя из RNFR

	say("220 fake ftp")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg, _ := strings.Cut(line, " ")
		verb = strings.ToUpper(verb)

		switch verb {
		case "AUTH":
			if s.tlsCfg == nil {
				say("500 not understood")
				continue
			}
			say("234 proceed with negotiation")
			tc := tls.Server(conn, s.tlsCfg)
			if err := tc.Handshake(); err != nil {
				s.t.Logf("сервер: рукопожатие не удалось: %v", err)
				return
			}
			conn, r, authed = tc, bufio.NewReader(tc), true
		case "PBSZ", "PROT":
			say("200 ok")
		case "USER":
			if !authed {
				say("530 use AUTH TLS first")
				continue
			}
			say("331 need password")
		case "PASS":
			if arg != "secret" {
				say("530 login incorrect")
				return
			}
			say("230 logged in")
		case "TYPE":
			say("200 ok")
		case "CWD":
			next := s.resolve(cwd, arg)
			s.mu.Lock()
			ok := s.dirs[next]
			s.mu.Unlock()
			if !ok {
				say("550 no such directory")
				continue
			}
			cwd = next
			say("250 ok")
		case "MKD":
			next := s.resolve(cwd, arg)
			s.mu.Lock()
			s.dirs[next] = true
			s.made = append(s.made, next)
			s.mu.Unlock()
			say("257 %q created", next)
		case "EPSV":
			if s.noEPSV {
				say("500 not understood")
				continue
			}
			ln, port := s.listenData()
			dataLn = ln
			say("229 Entering Extended Passive Mode (|||%d|)", port)
		case "PASV":
			ln, port := s.listenData()
			dataLn = ln
			say("227 Entering Passive Mode (127,0,0,1,%d,%d)", port/256, port%256)
		case "STOR":
			if dataLn == nil {
				say("425 use PASV first")
				continue
			}
			say("150 ok to send")
			dc, err := dataLn.Accept()
			dataLn.Close()
			dataLn = nil
			if err != nil {
				say("426 failed")
				continue
			}
			if s.tlsCfg != nil { // PROT P — канал данных тоже шифруется
				dc = tls.Server(dc, s.tlsCfg)
			}
			buf := make([]byte, 0, 1<<16)
			tmp := make([]byte, 32*1024)
			for {
				n, err := dc.Read(tmp)
				buf = append(buf, tmp[:n]...)
				if err != nil {
					break
				}
			}
			dc.Close()
			s.mu.Lock()
			s.files[s.resolve(cwd, arg)] = buf
			s.mu.Unlock()
			say("226 transfer complete")
		case "DELE":
			p := s.resolve(cwd, arg)
			s.mu.Lock()
			_, ok := s.files[p]
			delete(s.files, p)
			s.mu.Unlock()
			if !ok {
				say("550 no such file")
				continue
			}
			say("250 deleted")
		case "RNFR":
			p := s.resolve(cwd, arg)
			s.mu.Lock()
			_, ok := s.files[p]
			s.mu.Unlock()
			if !ok {
				say("550 no such file")
				continue
			}
			pending = p
			say("350 ready for RNTO")
		case "RNTO":
			if pending == "" {
				say("503 bad sequence")
				continue
			}
			s.mu.Lock()
			s.files[s.resolve(cwd, arg)] = s.files[pending]
			delete(s.files, pending)
			s.mu.Unlock()
			pending = ""
			say("250 renamed")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 not implemented")
		}
	}
}

func (s *fakeFTP) listenData() (net.Listener, int) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.t.Fatal(err)
	}
	port, _ := strconv.Atoi(strings.TrimPrefix(ln.Addr().String(), "127.0.0.1:"))
	return ln, port
}

func (s *fakeFTP) resolve(cwd, name string) string {
	if strings.HasPrefix(name, "/") {
		return path.Clean(name)
	}
	return path.Join(cwd, name)
}

func (s *fakeFTP) file(p string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.files[p]
}

func testSpec() *OMap {
	// В примере ответа прячется "</script>" — страница не должна от него порваться.
	return NewM().
		Set("openapi", "3.0.3").
		Set("info", NewM().Set("title", "Kaspi Shop API").Set("version", "1.0")).
		Set("paths", NewM().Set("/api/orders", NewM().Set("get", NewM().
			Set("summary", "Заказы </script><script>alert(1)</script>"))))
}

func testConfig(t *testing.T, s *fakeFTP) *Config {
	t.Helper()
	return &Config{
		Dir:     t.TempDir(),
		FTPHost: s.addr,
		FTPDir:  "/public_html/kaspi",
		FTPFile: "kaspi-api.html",
		FTPUser: "user",
		FTPPass: "secret",
		FTPTLS:  "off",
		FTPJSON: true,
	}
}

func writeSpecFile(t *testing.T, cfg *Config, spec *OMap) string {
	t.Helper()
	p := filepath.Join(cfg.Dir, "openapi.json")
	if err := writeJSON(p, spec); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPublishUploadsPageAndSpec(t *testing.T) {
	s := startFakeFTP(t, false)
	cfg := testConfig(t, s)
	spec := testSpec()
	specPath := writeSpecFile(t, cfg, spec)

	if !publish(cfg, spec, specPath) {
		t.Fatal("publish вернул false, ожидалась успешная заливка")
	}

	page := s.file("/public_html/kaspi/kaspi-api.html")
	if len(page) == 0 {
		t.Fatal("html на сервер не попал")
	}
	html := string(page)
	for _, want := range []string{
		"<title>Kaspi Shop API</title>",
		`id="openapi-spec"`,
		"SwaggerUIBundle",
		`href="kaspi-api.json"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("в странице нет %q", want)
		}
	}
	// Теги <script> только наши четыре: вставленный JSON заэкранирован,
	// разметку из примеров документации браузер увидеть не должен.
	if n := strings.Count(html, "<script"); n != 4 {
		t.Errorf("тегов <script> %d, ожидалось 4 — JSON протёк в разметку", n)
	}
	if strings.Contains(html, "<script>alert(1)") {
		t.Error("содержимое спецификации попало в разметку неэкранированным")
	}
	if !strings.Contains(html, backslash+"u003c/script"+backslash+"u003e") {
		t.Error("опасный фрагмент спецификации не заэкранирован")
	}

	spec2 := s.file("/public_html/kaspi/kaspi-api.json")
	raw, _ := os.ReadFile(specPath)
	if string(spec2) != string(raw) {
		t.Error("openapi.json на сервере отличается от локального")
	}
	if _, err := os.Stat(filepath.Join(cfg.Dir, "published.json")); err != nil {
		t.Errorf("не записана отметка о заливке: %v", err)
	}
	// Каталога не было — клиент должен был его создать.
	if len(s.made) == 0 {
		t.Error("недостающий каталог не создан")
	}
}

func TestPublishSkipsWhenUnchanged(t *testing.T) {
	s := startFakeFTP(t, false)
	cfg := testConfig(t, s)
	spec := testSpec()
	specPath := writeSpecFile(t, cfg, spec)

	if !publish(cfg, spec, specPath) {
		t.Fatal("первая заливка не прошла")
	}
	if publish(cfg, spec, specPath) {
		t.Error("вторая заливка не должна была состояться: спецификация та же")
	}
	cfg.FTPAlways = true
	if !publish(cfg, spec, specPath) {
		t.Error("с -ftp-always заливка должна повторяться")
	}
}

func TestPublishFallsBackToPASV(t *testing.T) {
	s := startFakeFTP(t, true) // EPSV отключён
	cfg := testConfig(t, s)
	spec := testSpec()
	specPath := writeSpecFile(t, cfg, spec)

	if !publish(cfg, spec, specPath) {
		t.Fatal("заливка через PASV не прошла")
	}
	if len(s.file("/public_html/kaspi/kaspi-api.html")) == 0 {
		t.Error("html на сервер не попал")
	}
}

func TestPublishOverFTPS(t *testing.T) {
	s := startFakeFTPTLS(t, false, &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}})
	cfg := testConfig(t, s)
	cfg.FTPTLS = "explicit"
	cfg.FTPSkipVerify = true // сертификат самоподписанный
	spec := testSpec()

	if !publish(cfg, spec, writeSpecFile(t, cfg, spec)) {
		t.Fatal("заливка по FTPS не прошла")
	}
	if len(s.file("/public_html/kaspi/kaspi-api.html")) == 0 {
		t.Error("html на сервер не попал")
	}
}

func TestPublishRejectsCertByDefault(t *testing.T) {
	s := startFakeFTPTLS(t, false, &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}})
	cfg := testConfig(t, s)
	cfg.FTPTLS = "explicit" // FTP_INSECURE не выставлен — сертификат должен проверяться
	spec := testSpec()

	if publish(cfg, spec, writeSpecFile(t, cfg, spec)) {
		t.Error("самоподписанный сертификат не должен приниматься молча")
	}
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestPublishBadPassword(t *testing.T) {
	s := startFakeFTP(t, false)
	cfg := testConfig(t, s)
	cfg.FTPPass = "wrong"
	spec := testSpec()
	if publish(cfg, spec, writeSpecFile(t, cfg, spec)) {
		t.Error("с неверным паролем заливка должна провалиться")
	}
}

func TestPublishDryRunWritesLocally(t *testing.T) {
	s := startFakeFTP(t, false)
	cfg := testConfig(t, s)
	cfg.DryRun = true
	spec := testSpec()
	if publish(cfg, spec, writeSpecFile(t, cfg, spec)) {
		t.Error("в dry-run ничего заливать нельзя")
	}
	for _, f := range []string{"kaspi-api.html", "kaspi-api.json"} {
		if _, err := os.Stat(filepath.Join(cfg.Dir, f)); err != nil {
			t.Errorf("в dry-run %s должен сохраняться локально: %v", f, err)
		}
	}
	if len(s.file("/public_html/kaspi/kaspi-api.html")) != 0 {
		t.Error("в dry-run файл ушёл на сервер")
	}
}
