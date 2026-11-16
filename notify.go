package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"time"
)

type Notifier interface {
	Notify(ctx context.Context, subject, body string) error
}

// Multi sends to every notifier and joins their errors.
type Multi []Notifier

func (m Multi) Notify(ctx context.Context, subject, body string) error {
	var errs []error
	for _, n := range m {
		errs = append(errs, n.Notify(ctx, subject, body))
	}
	return errors.Join(errs...)
}

// Webhook POSTs {"subject": ..., "body": ...} as JSON.
type Webhook struct {
	URL    string
	Bearer string
	HTTP   *http.Client
}

func (w Webhook) Notify(ctx context.Context, subject, body string) error {
	b, _ := json.Marshal(map[string]string{"subject": subject, "body": body})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if w.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+w.Bearer)
	}
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("webhook: HTTP %d: %s", resp.StatusCode, msg)
	}
	return nil
}

// SMTP sends a plain-text mail over a connection that must use TLS, either
// implicit (port 465) or STARTTLS.
type SMTP struct {
	Addr      string // host:port
	Username  string
	Password  string
	From      string
	To        []string
	Implicit  bool
	TLSConfig *tls.Config // nil: verify against the host name
}

func (s SMTP) Notify(ctx context.Context, subject, body string) error {
	host, _, err := net.SplitHostPort(s.Addr)
	if err != nil {
		return err
	}
	cfg := s.TLSConfig
	if cfg == nil {
		cfg = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	}
	d := net.Dialer{Timeout: 20 * time.Second}
	var conn net.Conn
	if s.Implicit {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: cfg}).DialContext(ctx, "tcp", s.Addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", s.Addr)
	}
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if !s.Implicit {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("smtp: server does not offer STARTTLS; refusing to send credentials in the clear")
		}
		if err := c.StartTLS(cfg); err != nil {
			return err
		}
	}
	if s.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, host)); err != nil {
			return err
		}
	}
	if err := c.Mail(s.From); err != nil {
		return err
	}
	for _, to := range s.To {
		if err := c.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	msg := "From: " + s.From + "\r\n" +
		"To: " + strings.Join(s.To, ", ") + "\r\n" +
		"Subject: " + strings.NewReplacer("\r", " ", "\n", " ").Replace(subject) + "\r\n" +
		"Date: " + time.Now().Format(time.RFC1123Z) + "\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		strings.ReplaceAll(body, "\n", "\r\n") + "\r\n"
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
