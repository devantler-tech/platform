package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"syscall"
	"time"
)

// privateFile only reads regular, non-executable files in the selected directory.
// The root anchors all member reads; leaf symlinks are never followed.
func privateFile(root *os.Root, name string, limit int64) ([]byte, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errConfiguration
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o137 != 0 || info.Size() > limit {
		return nil, errConfiguration
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (owner.Uid != 0 && owner.Uid != uint32(os.Geteuid())) {
		return nil, errConfiguration
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errConfiguration
	}
	if err = file.Close(); err != nil {
		return nil, errConfiguration
	}
	return raw, nil
}

func parseSettings(raw []byte) (settings, error) {
	var result settings
	fields := map[string]*string{"listenAddress": &result.ListenAddress, "sender": &result.Sender, "senderName": &result.SenderName, "clientUser": &result.ClientUser, "upstreamAddress": &result.UpstreamAddress, "upstreamServerName": &result.UpstreamServerName, "upstreamUser": &result.UpstreamUser, "replyTo": &result.ReplyTo}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return result, errConfiguration
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return result, errConfiguration
		}
		key, ok := token.(string)
		destination, known := fields[key]
		if !ok || !known || seen[key] {
			return result, errConfiguration
		}
		seen[key] = true
		value, valueErr := decoder.Token()
		text, stringValue := value.(string)
		if valueErr != nil || !stringValue {
			return result, errConfiguration
		}
		*destination = text
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return result, errConfiguration
	}
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return result, errConfiguration
	}
	return result, nil
}

func trustCertificates(raw []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	count := 0
	for len(bytes.TrimSpace(raw)) > 0 {
		// pem.Decode otherwise silently skips a non-PEM prefix.
		raw = bytes.TrimSpace(raw)
		if !bytes.HasPrefix(raw, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, errConfiguration
		}
		block, remaining := pem.Decode(raw)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, errConfiguration
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA {
			return nil, errConfiguration
		}
		pool.AddCert(cert)
		count++
		raw = remaining
	}
	if count == 0 {
		return nil, errConfiguration
	}
	return pool, nil
}

func loadConfiguration(directory string) (cfg configuration, resultErr error) {
	if directory == "" {
		return cfg, errConfiguration
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return cfg, errConfiguration
	}
	defer func(opened *os.Root) {
		if closeErr := opened.Close(); closeErr != nil {
			resultErr = errConfiguration
		}
	}(root)
	// Kubernetes projects each Secret generation under ..data. Pin that directory
	// once, before reading any members, rather than mixing two generations.
	if _, err = root.Lstat("..data"); err == nil {
		snapshot, e := root.OpenRoot("..data")
		if e != nil {
			return cfg, errConfiguration
		}
		defer func(opened *os.Root) {
			if closeErr := opened.Close(); closeErr != nil {
				resultErr = errConfiguration
			}
		}(snapshot)
		root = snapshot
	} else if !errors.Is(err, os.ErrNotExist) {
		return cfg, errConfiguration
	}
	members := make(map[string][]byte)
	for _, name := range []string{"settings.json", "client-password", "upstream-password", "server.crt", "server.key", "upstream-ca.pem"} {
		limit := int64(64 << 10)
		if name == "settings.json" {
			limit = 4 << 10
		}
		members[name], err = privateFile(root, name, limit)
		if err != nil {
			return cfg, errConfiguration
		}
	}
	cfg.Settings, err = parseSettings(members["settings.json"])
	if err != nil {
		return cfg, errConfiguration
	}
	cfg.ClientPassword = string(members["client-password"])
	cfg.UpstreamPassword = string(members["upstream-password"])
	pair, err := tls.X509KeyPair(members["server.crt"], members["server.key"])
	if err != nil {
		return cfg, errConfiguration
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return cfg, errConfiguration
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return cfg, errConfiguration
	}
	roots, err := trustCertificates(members["upstream-ca.pem"])
	if err != nil {
		return cfg, errConfiguration
	}
	cfg.ServerTLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	cfg.UpstreamTLS = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: cfg.Settings.UpstreamServerName}
	return cfg, cfg.validate()
}
