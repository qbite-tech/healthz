package alert

import (
	"context"
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"os"
	"strings"
	"time"

	"github.com/qbite-tech/healthz/internal/config"
)

type Message struct {
	Subject    string
	Body       string
	Recipients []string
}

type Sender interface {
	Send(context.Context, Message) error
}

type EmailSender struct {
	config config.SMTPConfig
}

func NewEmailSender(cfg config.SMTPConfig) *EmailSender {
	return &EmailSender{config: cfg}
}

func (s *EmailSender) Send(ctx context.Context, message Message) error {
	if !s.config.Enabled {
		return nil
	}

	address := net.JoinHostPort(s.config.Host, fmt.Sprintf("%d", s.config.Port))
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var connection net.Conn
	var err error
	tlsConfig := &tls.Config{ServerName: s.config.Host, MinVersion: tls.VersionTLS12}
	if s.config.TLS == "implicit" {
		connection, err = tls.DialWithDialer(dialer, "tcp", address, tlsConfig)
	} else {
		connection, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return fmt.Errorf("connect to SMTP server: %w", err)
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	} else {
		_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	}

	client, err := smtp.NewClient(connection, s.config.Host)
	if err != nil {
		return fmt.Errorf("start SMTP client: %w", err)
	}
	defer client.Close()

	if s.config.TLS == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("SMTP server does not support STARTTLS")
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("enable STARTTLS: %w", err)
		}
	}
	if s.config.Username != "" {
		password, exists := os.LookupEnv(s.config.PasswordEnv)
		if !exists {
			return fmt.Errorf("SMTP password environment variable %s is not set", s.config.PasswordEnv)
		}
		auth := smtp.PlainAuth("", s.config.Username, password, s.config.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("authenticate to SMTP server: %w", err)
		}
	}
	recipients := messageRecipients(s.config, message)
	if err := client.Mail(s.config.From); err != nil {
		return fmt.Errorf("set SMTP sender: %w", err)
	}
	for _, recipient := range recipients {
		if err := client.Rcpt(recipient); err != nil {
			return fmt.Errorf("set SMTP recipient %s: %w", recipient, err)
		}
	}

	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("start SMTP message: %w", err)
	}
	payload := buildMessage(s.config, recipients, message)
	if _, err := writer.Write([]byte(payload)); err != nil {
		_ = writer.Close()
		return fmt.Errorf("write SMTP message: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish SMTP message: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("quit SMTP session: %w", err)
	}
	return nil
}

func messageRecipients(cfg config.SMTPConfig, message Message) []string {
	if len(message.Recipients) > 0 {
		return message.Recipients
	}
	return cfg.To
}

func buildMessage(cfg config.SMTPConfig, recipients []string, message Message) string {
	subject := mime.QEncoding.Encode("utf-8", cleanHeader(message.Subject))
	return "From: " + cleanHeader(cfg.From) + "\r\n" +
		"To: " + strings.Join(cleanHeaders(recipients), ", ") + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"Content-Transfer-Encoding: 8bit\r\n" +
		"\r\n" + normalizeCRLF(message.Body) + "\r\n"
}

func cleanHeaders(values []string) []string {
	cleaned := make([]string, len(values))
	for i, value := range values {
		cleaned[i] = cleanHeader(value)
	}
	return cleaned
}

func cleanHeader(value string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(value)
}

func normalizeCRLF(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.ReplaceAll(value, "\n", "\r\n")
}
