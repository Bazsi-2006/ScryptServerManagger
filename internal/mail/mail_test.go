package mail

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/logging"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/testutil"
)

// fakeSMTP is a minimal SMTP server that records one message.
type fakeSMTP struct {
	ln   net.Listener
	mu   sync.Mutex
	from string
	to   string
	data string
}

func startSMTP(t *testing.T) *fakeSMTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	w := func(line string) { c.Write([]byte(line + "\r\n")) }
	w("220 fake ESMTP")
	inData := false
	var body strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				s.mu.Lock()
				s.data = body.String()
				s.mu.Unlock()
				w("250 queued")
				continue
			}
			body.WriteString(line + "\n")
			continue
		}
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			w("250 fake")
		case strings.HasPrefix(up, "MAIL FROM:"):
			s.mu.Lock()
			s.from = line[10:]
			s.mu.Unlock()
			w("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			s.mu.Lock()
			s.to = line[8:]
			s.mu.Unlock()
			w("250 ok")
		case up == "DATA":
			inData = true
			w("354 go ahead")
		case up == "QUIT":
			w("221 bye")
			return
		default:
			w("502 unsupported")
		}
	}
}

func TestSendOverSMTP(t *testing.T) {
	srv := startSMTP(t)
	cfg := testutil.Config(t)
	addr := srv.ln.Addr().(*net.TCPAddr)
	cfg.Update(func(c *config.Config) error {
		c.Mode = config.ModeProduction
		c.SMTP = config.SMTP{Host: "127.0.0.1", Port: addr.Port, From: "Server Manager <sm@example.com>", TLS: "none"}
		return nil
	})
	m := New(cfg, logging.Discard())
	out, err := m.Send(context.Background(), Message{To: "user@example.com", Subject: "Einladung für dich", Body: "Hello\nhttps://sm.example.com/#/register?token=abc\n"})
	if err != nil || out != Sent {
		t.Fatalf("send: %v %v", out, err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !strings.Contains(srv.from, "sm@example.com") || !strings.Contains(srv.to, "user@example.com") {
		t.Fatalf("envelope: %q -> %q", srv.from, srv.to)
	}
	for _, want := range []string{"To: user@example.com", "Subject: =?utf-8?q?Einladung_f=C3=BCr_dich?=", "Content-Type: text/plain; charset=utf-8", "token=abc"} {
		if !strings.Contains(srv.data, want) {
			t.Errorf("message lacks %q:\n%s", want, srv.data)
		}
	}
}

func TestHeaderInjectionIsRejected(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.Update(func(c *config.Config) error {
		c.SMTP = config.SMTP{Host: "127.0.0.1", Port: 1, From: "sm@example.com", TLS: "none"}
		return nil
	})
	m := New(cfg, logging.Discard())
	if _, err := m.Send(context.Background(), Message{To: "a@example.com", Subject: "hi\r\nBcc: victim@example.com", Body: "x"}); err == nil {
		t.Fatal("CR/LF in the subject must be rejected")
	}
	if _, err := m.Send(context.Background(), Message{To: "a@example.com\r\nBcc: v@example.com", Subject: "s", Body: "x"}); err == nil {
		t.Fatal("CR/LF in the recipient must be rejected")
	}
}

func TestUnconfiguredDelivery(t *testing.T) {
	cfg := testutil.Config(t) // development, no SMTP
	m := New(cfg, logging.Discard())
	if out, err := m.Send(context.Background(), Message{To: "a@example.com", Subject: "s", Body: "b"}); err != nil || out != Logged {
		t.Fatalf("development mode should log: %v %v", out, err)
	}
	cfg.Update(func(c *config.Config) error { c.Mode = config.ModeProduction; return nil })
	if _, err := m.Send(context.Background(), Message{To: "a@example.com", Subject: "s", Body: "b"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("production without SMTP: %v", err)
	}
	if m.Available() {
		t.Fatal("email is unavailable in production without SMTP")
	}
	// STARTTLS is mandatory when configured: a server without it is refused.
	srv := startSMTP(t)
	cfg.Update(func(c *config.Config) error {
		c.SMTP = config.SMTP{Host: "127.0.0.1", Port: srv.ln.Addr().(*net.TCPAddr).Port, From: "sm@example.com", TLS: "starttls"}
		return nil
	})
	if _, err := m.Send(context.Background(), Message{To: "a@example.com", Subject: "s", Body: "b"}); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("missing STARTTLS must fail closed: %v", err)
	}
}
