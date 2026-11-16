// Package mail delivers email over SMTP, or to the application log in
// development mode when SMTP is not configured.
package mail

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	netmail "net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
)

// ErrNotConfigured means SMTP is not set up (and this is not development
// mode, where messages are logged instead).
var ErrNotConfigured = errors.New("email delivery is not configured")

type Message struct {
	To      string
	Subject string
	Body    string
}

// Outcome tells the caller what happened to a message.
type Outcome int

const (
	// Sent: accepted by the SMTP server.
	Sent Outcome = iota
	// Logged: written to the application log (development mode only).
	Logged
)

type Mailer struct {
	cfg *config.Holder
	log *slog.Logger
}

func New(cfg *config.Holder, log *slog.Logger) *Mailer { return &Mailer{cfg: cfg, log: log} }

// Available reports whether Send can do anything useful right now.
func (m *Mailer) Available() bool {
	c := m.cfg.Get()
	return c.SMTP.Configured() || c.Development()
}

func build(from string, msg Message) ([]byte, error) {
	if strings.ContainsAny(msg.To, "\r\n") || strings.ContainsAny(msg.Subject, "\r\n") {
		return nil, errors.New("invalid characters in message headers")
	}
	fromAddr, err := netmail.ParseAddress(from)
	if err != nil {
		return nil, fmt.Errorf("invalid sender address: %w", err)
	}
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	domain := "localhost"
	if i := strings.LastIndexByte(fromAddr.Address, '@'); i >= 0 {
		domain = fromAddr.Address[i+1:]
	}
	var b strings.Builder
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", fromAddr.String())
	h("To", msg.To)
	h("Subject", mime.QEncoding.Encode("utf-8", msg.Subject))
	h("Date", time.Now().Format(time.RFC1123Z))
	h("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Content-Transfer-Encoding", "8bit")
	h("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(msg.Body, "\r\n", "\n"), "\n", "\r\n"))
	return []byte(b.String()), nil
}

// Send delivers msg. In development mode without SMTP configuration the
// message (including any links it contains) is written to the log so it can
// be inspected; production never logs message bodies.
func (m *Mailer) Send(ctx context.Context, msg Message) (Outcome, error) {
	c := m.cfg.Get()
	to, err := netmail.ParseAddress(msg.To)
	if err != nil {
		return Sent, fmt.Errorf("invalid recipient address")
	}
	if !c.SMTP.Configured() {
		if c.Development() {
			m.log.Info("development mode: email not sent, printing it instead",
				"to", to.Address, "subject", msg.Subject, "body", msg.Body)
			return Logged, nil
		}
		return Sent, ErrNotConfigured
	}
	data, err := build(c.SMTP.From, msg)
	if err != nil {
		return Sent, err
	}
	password, err := c.SMTPPassword()
	if err != nil {
		return Sent, err
	}
	from, _ := netmail.ParseAddress(c.SMTP.From)
	if err := send(ctx, c.SMTP, password, from.Address, to.Address, data); err != nil {
		return Sent, fmt.Errorf("SMTP delivery failed: %w", err)
	}
	return Sent, nil
}

func send(ctx context.Context, s config.SMTP, password, from, to string, data []byte) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	tlsCfg := &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	var err error
	d := &net.Dialer{}
	if s.TLS == "tls" {
		conn, err = (&tls.Dialer{NetDialer: d, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		return err
	}
	defer c.Close()
	if s.TLS == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("server does not offer STARTTLS (set smtp.tls to \"none\" only for trusted local relays)")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if s.Username != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("server does not offer authentication")
		}
		// PlainAuth refuses to send credentials over an unencrypted
		// connection to anything but localhost.
		if err := c.Auth(smtp.PlainAuth("", s.Username, password, s.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
