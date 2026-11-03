// Exercise the rendered diagnostic with real curl and synthetic loopback peers.
package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func serve(listener net.Listener, proxy, secure bool, results chan<- error) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		func() {
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			reader := bufio.NewReader(conn)
			if proxy {
				header, err := reader.ReadString('\n')
				if err != nil || !strings.HasPrefix(header, "PROXY TCP4 ") || !strings.HasSuffix(header, "\r\n") || len(strings.Fields(header)) != 6 {
					results <- fmt.Errorf("missing valid PROXY preamble")
					return
				}
			}
			if secure {
				header := make([]byte, 5)
				if _, err := io.ReadFull(reader, header); err != nil || header[0] != 22 || header[1] != 3 {
					results <- fmt.Errorf("missing TLS ClientHello after transport framing")
					return
				}
				// Deliberately refuse before the handshake, just as a denied physical peer is closed.
				results <- nil
				return
			}
			line, err := reader.ReadString('\n')
			if err != nil || line != "GET / HTTP/1.1\r\n" {
				results <- fmt.Errorf("missing HTTP request after transport framing")
				return
			}
			_, err = io.WriteString(conn, "HTTP/1.1 301 Moved Permanently\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			results <- err
		}()
	}
}

func run() error {
	if len(os.Args) != 3 || os.Args[2] != "plain" && os.Args[2] != "proxy-v1" {
		return fmt.Errorf("arguments")
	}
	httpPeer, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer func() { _ = httpPeer.Close() }()
	tlsPeer, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer func() { _ = tlsPeer.Close() }()
	results := make(chan error, 32)
	go serve(httpPeer, os.Args[2] == "proxy-v1", false, results)
	go serve(tlsPeer, os.Args[2] == "proxy-v1", true, results)
	curl, err := exec.LookPath("curl")
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "gateway-denied-wire-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()
	wrapper := "#!/bin/sh\nexec \"$WIRE_CURL\" --noproxy '*' --connect-to \"$GATEWAY_IP:80:$WIRE_HTTP\" --connect-to \"$TLS_HOST:443:$WIRE_TLS\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(work, "curl"), []byte(wrapper), 0700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "sh", os.Args[1])
	command.Env = append(os.Environ(), "PATH="+work+":"+os.Getenv("PATH"), "WIRE_CURL="+curl,
		"WIRE_HTTP="+httpPeer.Addr().String(), "WIRE_TLS="+tlsPeer.Addr().String(),
		"ATTEMPTS=2", "WARMUP_ATTEMPTS=1", "TIMEOUT=1", "SETTLE_SECONDS=0",
		"GATEWAY_IP=192.0.2.1", "TLS_HOST=example.test", "CONTROL_HOST=x.invalid")
	output, err := command.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("PROBE control-before 301\n")) || !bytes.Contains(output, []byte("PROBE control-after 301\n")) {
		return fmt.Errorf("native controls failed: %v: %s", err, output)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.HasPrefix(line, "PROBE tls ") && !strings.HasPrefix(line, "PROBE tls 35 000 0.000000") && !strings.HasPrefix(line, "PROBE tls 56 000 0.000000") {
			return fmt.Errorf("native TLS refusal: %s", line)
		}
	}
	for i := 0; i < 4; i++ {
		select {
		case err := <-results:
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("native request count below four")
		}
	}
	select {
	case <-results:
		return fmt.Errorf("unexpected extra request")
	default:
	}
	fmt.Printf("GATEWAY_DIAGNOSTIC_WIRE=PASS transport=%s requests=4\n", os.Args[2])
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
