package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"golang.org/x/time/rate"
)

const (
	maxMessageBytes  = 1 << 20
	maxHeaderBytes   = 64 << 10
	maxConnections   = 16
	maxAuthAttempts  = 3
	sessionLifetime  = 30 * time.Second
	upstreamLifetime = 10 * time.Second
)

var (
	errConfiguration     = errors.New("submission configuration refused")
	errUpstreamTemporary = errors.New("upstream temporarily unavailable")
	errUpstreamPermanent = errors.New("upstream refused")
)

type settings struct {
	ListenAddress      string
	Sender             string
	SenderName         string
	ClientUser         string
	UpstreamAddress    string
	UpstreamServerName string
	UpstreamUser       string
	ReplyTo            string
}

type configuration struct {
	Settings         settings
	ClientPassword   string
	UpstreamPassword string
	ServerTLS        *tls.Config
	UpstreamTLS      *tls.Config
}

type gateway struct {
	server                *smtp.Server
	cfg                   configuration
	limiter               *rate.Limiter
	authenticationLimiter *rate.Limiter
	logger                *log.Logger
}

// safeSMTPLogger deliberately discards library error arguments, which may contain
// peer addresses or other protocol input. Wire debugging is never enabled.
type safeSMTPLogger struct{ logger *log.Logger }

func (s safeSMTPLogger) Printf(_ string, _ ...any) { s.logger.Print("connection refused") }
func (s safeSMTPLogger) Println(_ ...any)          { s.logger.Print("connection refused") }

func ascii(value string, minimum, maximum int, spaces bool) bool {
	if len(value) < minimum || len(value) > maximum || strings.Contains(value, "${") || strings.Contains(value, "{{") {
		return false
	}
	for _, r := range value {
		if r < 32 || r > 126 || (!spaces && r == ' ') {
			return false
		}
	}
	return strings.TrimSpace(value) != ""
}

func mailbox(value string) bool {
	if !ascii(value, 3, 254, false) || strings.ContainsAny(value, ":;,<>\"\\[]") {
		return false
	}
	address, err := mail.ParseAddress(value)
	return err == nil && address.Name == "" && address.Address == value
}

func endpoint(value string, listening bool) bool {
	host, port, err := net.SplitHostPort(value)
	number, e := strconv.Atoi(port)
	if err != nil || e != nil || host == "" || !ascii(host, 1, 253, false) || number < 1 || number > 65535 {
		return false
	}
	return !listening || (net.ParseIP(host) != nil && number >= 1024)
}

func (cfg configuration) validate() error {
	s := cfg.Settings
	if !endpoint(s.ListenAddress, true) || !endpoint(s.UpstreamAddress, false) ||
		!mailbox(s.Sender) || (s.ReplyTo != "" && !mailbox(s.ReplyTo)) ||
		!ascii(s.SenderName, 1, 128, true) || strings.ContainsAny(s.SenderName, ":;,") ||
		!ascii(s.ClientUser, 1, 254, false) || !ascii(s.UpstreamUser, 1, 254, false) || s.ClientUser == s.UpstreamUser ||
		!ascii(cfg.ClientPassword, 32, 256, true) || !ascii(cfg.UpstreamPassword, 32, 256, true) || cfg.ClientPassword == cfg.UpstreamPassword ||
		!ascii(s.UpstreamServerName, 1, 253, false) || strings.ContainsAny(s.UpstreamServerName, "/:@") ||
		cfg.ServerTLS == nil || len(cfg.ServerTLS.Certificates) != 1 ||
		cfg.UpstreamTLS == nil || cfg.UpstreamTLS.InsecureSkipVerify || cfg.UpstreamTLS.RootCAs == nil ||
		cfg.UpstreamTLS.RootCAs.Equal(x509.NewCertPool()) || cfg.UpstreamTLS.ServerName != s.UpstreamServerName {
		return errConfiguration
	}
	return nil
}

func newGateway(cfg configuration, output io.Writer) (*gateway, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if output == nil {
		output = io.Discard
	}
	cfg.ServerTLS = cfg.ServerTLS.Clone()
	cfg.ServerTLS.MinVersion = tls.VersionTLS12
	cfg.UpstreamTLS = cfg.UpstreamTLS.Clone()
	cfg.UpstreamTLS.MinVersion = tls.VersionTLS12
	g := &gateway{cfg: cfg, limiter: rate.NewLimiter(rate.Every(6*time.Second), 10), authenticationLimiter: rate.NewLimiter(rate.Every(time.Second), 16), logger: log.New(output, "SMTP_SUBMISSION ", 0)}
	g.server = smtp.NewServer(g)
	g.server.Domain = "submission.invalid"
	g.server.ReadTimeout = 10 * time.Second
	g.server.WriteTimeout = 5 * time.Second
	// The DATA reader needs one extra byte to observe its terminator. The
	// application still enforces the exact payload bound in Mail and Data.
	g.server.MaxMessageBytes = maxMessageBytes + 1
	g.server.MaxRecipients = 1
	g.server.ErrorLog = safeSMTPLogger{g.logger}
	return g, nil
}

// serve supplies actual tls.Conn values so the SMTP library recognizes TLS and
// never offers insecure authentication. Deadlines and connection slots apply
// beneath TLS, including handshakes and clients that never authenticate.
func (g *gateway) serve(listener net.Listener) error {
	return g.server.Serve(newTLSListener(listener, g.cfg.ServerTLS))
}

func (g *gateway) NewSession(conn *smtp.Conn) (smtp.Session, error) {
	state, ok := conn.TLSConnectionState()
	if !ok || !state.HandshakeComplete || state.Version < tls.VersionTLS12 {
		return nil, smtp.ErrAuthRequired
	}
	return &session{gateway: g}, nil
}

type session struct {
	gateway       *gateway
	authenticated bool
	attempts      int
	from          string
	recipient     string
}

// credentialsEqual compares provisioned machine secrets, not password hashes.
// Padding keeps comparison work independent of either credential length.
func credentialsEqual(actual, expected string) bool {
	const maximum = 256
	if len(actual) > maximum || len(expected) > maximum {
		return false
	}
	var a, b [maximum]byte
	copy(a[:], actual)
	copy(b[:], expected)
	return subtle.ConstantTimeEq(int32(len(actual)), int32(len(expected)))&subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func (s *session) AuthMechanisms() []string { return []string{sasl.Plain} }
func (s *session) Auth(_ string) (sasl.Server, error) {
	s.attempts++
	if s.attempts > maxAuthAttempts || !s.gateway.authenticationLimiter.Allow() {
		return nil, smtp.ErrAuthFailed
	}
	return sasl.NewPlainServer(func(identity, user, password string) error {
		if identity != "" || user != s.gateway.cfg.Settings.ClientUser || !credentialsEqual(password, s.gateway.cfg.ClientPassword) {
			return smtp.ErrAuthFailed
		}
		s.authenticated = true
		return nil
	}), nil
}

func refusal(code int, message string) error {
	return &smtp.SMTPError{Code: code, Message: message}
}

func (s *session) Mail(from string, options *smtp.MailOptions) error {
	if !s.authenticated {
		return smtp.ErrAuthRequired
	}
	s.Reset()
	if from != s.gateway.cfg.Settings.Sender {
		return refusal(550, "Sender refused")
	}
	if options != nil && options.Size > maxMessageBytes {
		return smtp.ErrDataTooLarge
	}
	s.from = from
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	if !s.authenticated || s.from == "" {
		return smtp.ErrAuthRequired
	}
	if s.recipient != "" || !mailbox(to) {
		return refusal(550, "Recipient refused")
	}
	s.recipient = to
	return nil
}

func oneAddress(values []string, address, name string, checkName bool) bool {
	if len(values) != 1 || strings.ContainsAny(values[0], ":;,") {
		return false
	}
	parsed, err := mail.ParseAddress(values[0])
	return err == nil && parsed.Address == address && (!checkName || parsed.Name == "" || parsed.Name == name)
}

func (s *session) validMessage(raw []byte) bool {
	end := bytes.Index(raw, []byte("\r\n\r\n"))
	if end < 0 || end > maxHeaderBytes {
		return false
	}
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil || !oneAddress(message.Header["From"], s.from, s.gateway.cfg.Settings.SenderName, true) {
		return false
	}
	for key := range message.Header {
		lower := strings.ToLower(key)
		if lower == "sender" || lower == "return-path" || strings.HasPrefix(lower, "resent-") {
			return false
		}
	}
	replies := message.Header["Reply-To"]
	if s.gateway.cfg.Settings.ReplyTo == "" {
		return len(replies) == 0
	}
	return oneAddress(replies, s.gateway.cfg.Settings.ReplyTo, "", false)
}

func (s *session) Data(reader io.Reader) error {
	if !s.authenticated || s.from == "" || s.recipient == "" {
		return smtp.ErrAuthRequired
	}
	permitted := s.gateway.limiter.Allow()
	raw, err := io.ReadAll(io.LimitReader(reader, maxMessageBytes+1))
	if err != nil || len(raw) > maxMessageBytes || !s.validMessage(raw) {
		return refusal(554, "Message refused")
	}
	if !permitted {
		return refusal(451, "Submission temporarily unavailable")
	}
	if err = s.forward(raw); err != nil {
		s.gateway.logger.Print("upstream refused")
		if errors.Is(err, errUpstreamPermanent) {
			return refusal(554, "Submission refused")
		}
		return refusal(451, "Submission temporarily unavailable")
	}
	s.gateway.logger.Print("accepted")
	return nil
}

func (s *session) forward(raw []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), upstreamLifetime)
	defer cancel()
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", s.gateway.cfg.Settings.UpstreamAddress)
	if err != nil {
		return errUpstreamTemporary
	}
	deadline, _ := ctx.Deadline()
	bounded := &deadlineConn{Conn: conn, expires: deadline}
	defer func() { _ = bounded.Close() }()
	client, err := smtp.NewClientStartTLS(bounded, s.gateway.cfg.UpstreamTLS)
	if err != nil {
		return errUpstreamTemporary
	}
	defer func() { _ = client.Close() }()
	client.CommandTimeout = 5 * time.Second
	client.SubmissionTimeout = 5 * time.Second
	if err = client.Auth(sasl.NewPlainClient("", s.gateway.cfg.Settings.UpstreamUser, s.gateway.cfg.UpstreamPassword)); err != nil {
		return errUpstreamTemporary
	}
	// SendMail waits for the final DATA acknowledgement. A later QUIT failure
	// must not turn an accepted message into a retry; closing the session suffices.
	err = client.SendMail(s.from, []string{s.recipient}, bytes.NewReader(raw))
	if err == nil {
		return nil
	}
	var protocolError *smtp.SMTPError
	if errors.As(err, &protocolError) && protocolError.Code >= 500 && protocolError.Code < 600 {
		return errUpstreamPermanent
	}
	return errUpstreamTemporary
}

func (s *session) Reset()        { s.from = ""; s.recipient = "" }
func (s *session) Logout() error { return nil }
