// This synthetic server is a test utility, never part of a published image.
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/server"
)

type fixture struct {
	u      backend.User
	source bool
	writes atomic.Int32
}

func (f *fixture) Login(_ *imap.ConnInfo, username, password string) (backend.User, error) {
	wanted := "ned@devantler.tech"
	if f.source {
		wanted = "owner@icloud.com"
	}
	if username != wanted || password != "password" {
		return nil, errors.New("fixture login")
	}
	if f.source {
		return &readOnlyUser{User: f.u, writes: &f.writes}, nil
	}
	return f.u, nil
}

type readOnlyUser struct {
	backend.User
	writes *atomic.Int32
}

func (u *readOnlyUser) refuse() error                      { u.writes.Add(1); return errors.New("source mutation") }
func (u *readOnlyUser) CreateMailbox(string) error         { return u.refuse() }
func (u *readOnlyUser) DeleteMailbox(string) error         { return u.refuse() }
func (u *readOnlyUser) RenameMailbox(string, string) error { return u.refuse() }
func (u *readOnlyUser) GetMailbox(name string) (backend.Mailbox, error) {
	m, err := u.User.GetMailbox(name)
	if err != nil {
		return nil, err
	}
	return &readOnlyMailbox{Mailbox: m, writes: u.writes}, nil
}

type readOnlyMailbox struct {
	backend.Mailbox
	writes *atomic.Int32
}

func (m *readOnlyMailbox) refuse() error                                         { m.writes.Add(1); return errors.New("source mutation") }
func (m *readOnlyMailbox) SetSubscribed(bool) error                              { return m.refuse() }
func (m *readOnlyMailbox) CreateMessage([]string, time.Time, imap.Literal) error { return m.refuse() }
func (m *readOnlyMailbox) UpdateMessagesFlags(bool, *imap.SeqSet, imap.FlagsOp, []string) error {
	return m.refuse()
}
func (m *readOnlyMailbox) CopyMessages(bool, *imap.SeqSet, string) error { return m.refuse() }
func (m *readOnlyMailbox) Expunge() error                                { return m.refuse() }

func message(id, body string) string {
	return "From: sender@example.invalid\r\nTo: recipient@example.invalid\r\nSubject: synthetic history\r\nMessage-ID: <" + id + "@example.invalid>\r\nDate: Fri, 01 May 2026 12:00:00 +0000\r\n\r\n" + body + "\r\n"
}

func add(u backend.User, box, id, body string, flags []string) error {
	m, err := u.GetMailbox(box)
	if err != nil {
		if err = u.CreateMailbox(box); err != nil {
			return err
		}
		m, err = u.GetMailbox(box)
	}
	if err != nil {
		return err
	}
	return m.CreateMessage(flags, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC), strings.NewReader(message(id, body)))
}

func snapshot(u backend.User) [32]byte {
	boxes, err := u.ListMailboxes(false)
	if err != nil {
		panic(err)
	}
	contents := map[string][]*memory.Message{}
	for _, box := range boxes {
		contents[box.Name()] = box.(*memory.Mailbox).Messages
	}
	data, err := json.Marshal(contents)
	if err != nil {
		panic(err)
	}
	return sha256.Sum256(data)
}

func serve(side string) error {
	b := memory.New()
	u, err := b.Login(nil, "username", "password")
	if err != nil {
		return err
	}
	f := &fixture{u: u, source: side == "source"}
	if f.source {
		if err = add(u, "INBOX", "second", "second-body-canary", []string{imap.FlaggedFlag, imap.DeletedFlag}); err != nil {
			return err
		}
		for range 2 {
			if err = add(u, "Archive/2024", "duplicate", "duplicate-body-canary", []string{imap.SeenFlag}); err != nil {
				return err
			}
		}
		if err = u.CreateMailbox("Empty"); err != nil {
			return err
		}
	} else {
		if err = add(u, "INBOX", "existing", "existing-body-canary", []string{imap.FlaggedFlag}); err != nil {
			return err
		}
		if err = add(u, "Keep", "keep", "keep-body-canary", []string{imap.SeenFlag}); err != nil {
			return err
		}
	}
	initial := snapshot(u)
	assert := http.NewServeMux()
	assert.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	assert.HandleFunc("/assert", func(w http.ResponseWriter, _ *http.Request) {
		ok := f.writes.Load() == 0
		if f.source {
			ok = ok && initial == snapshot(u)
		} else {
			for name, count := range map[string]int{"INBOX": 3, "Archive/2024": 2, "Empty": 0, "Keep": 1} {
				m, err := u.GetMailbox(name)
				if err != nil || len(m.(*memory.Mailbox).Messages) != count {
					ok = false
					continue
				}
			}
			m, _ := u.GetMailbox("INBOX")
			second, existing := false, false
			for _, msg := range m.(*memory.Mailbox).Messages {
				if bytes.Contains(msg.Body, []byte("second-body-canary")) {
					second = reflect.DeepEqual(msg.Flags, []string{imap.FlaggedFlag})
				}
				if bytes.Contains(msg.Body, []byte("existing-body-canary")) {
					existing = reflect.DeepEqual(msg.Flags, []string{imap.FlaggedFlag})
				}
			}
			ok = ok && second && existing
		}
		if !ok {
			w.WriteHeader(http.StatusConflict)
			return
		}
		_, _ = io.WriteString(w, "FIXTURE=PASS\n")
	})
	go func() { _ = http.ListenAndServe("127.0.0.1:8081", assert) }()
	cert, err := tls.LoadX509KeyPair("/fixture/server.pem", "/fixture/server.key")
	if err != nil {
		return err
	}
	listener, err := tls.Listen("tcp", ":993", &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}})
	if err != nil {
		return err
	}
	s := server.New(f)
	s.ErrorLog = log.New(io.Discard, "", 0)
	return s.Serve(listener)
}

func certificates(dir string) error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "history-test-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		return err
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"imap.mail.me.com", "mail.devantler.tech"}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		return err
	}
	for name, data := range map[string][]byte{"ca.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), "server.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), "server.key": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0400); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	if len(os.Args) != 3 {
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "certificates":
		err = certificates(os.Args[2])
	case "serve":
		err = serve(os.Args[2])
	case "probe":
		response, e := http.Get("http://127.0.0.1:8081/" + os.Args[2])
		if e != nil {
			err = e
		} else {
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				err = errors.New("fixture state")
			}
		}
	default:
		err = errors.New("fixture command")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "FIXTURE=FAIL", err)
		os.Exit(1)
	}
}
