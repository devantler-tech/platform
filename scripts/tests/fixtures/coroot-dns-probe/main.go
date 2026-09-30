// The probe observes real resolver requests against a loopback-only DNS server.
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	server, err := net.ListenPacket("udp4", "127.0.0.1:53")
	if err != nil {
		return err
	}
	defer server.Close()
	var mu sync.Mutex
	var queries []string
	go func() {
		buffer := make([]byte, 512)
		for {
			n, peer, err := server.ReadFrom(buffer)
			if err != nil {
				return
			}
			if n < 17 {
				continue
			}
			end := 12
			var labels []string
			for end < n && buffer[end] > 0 {
				length := int(buffer[end])
				if length > 63 || end+1+length >= n {
					break
				}
				labels = append(labels, string(buffer[end+1:end+1+length]))
				end += length + 1
			}
			if end+5 > n || buffer[end] != 0 {
				continue
			}
			name := strings.Join(labels, ".") + "."
			mu.Lock()
			queries = append(queries, name)
			mu.Unlock()
			reply := append([]byte(nil), buffer[:end+5]...)
			// One question, recursive response; unknown names get NXDOMAIN.
			binary.BigEndian.PutUint16(reply[2:4], 0x8183)
			clear(reply[6:12])
			valid := name == "coroot-clickhouse.observability.svc.cluster.local." ||
				name == "coroot-clickhouse-shard-0-0.coroot-clickhouse-headless.observability.svc.cluster.local." ||
				name == "telemetry.eu.example.com."
			if valid {
				binary.BigEndian.PutUint16(reply[2:4], 0x8180)
				if binary.BigEndian.Uint16(buffer[end+1:end+3]) == 1 {
					binary.BigEndian.PutUint16(reply[6:8], 1)
					// Compressed owner, A/IN, zero TTL, documentation-only IPv4.
					reply = append(reply, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 0, 0, 4, 192, 0, 2, 1)
				}
			}
			_, _ = server.WriteTo(reply, peer)
		}
	}()
	for _, resolver := range []struct{ name, family, database string }{
		{name: "go-ipv4", family: "ip4"},
		{name: "go-dual-stack", family: "ip"},
		{name: "libc-ipv4", database: "ahostsv4"},
		{name: "libc-dual-stack", database: "ahosts"},
	} {
		for _, name := range []string{
			"coroot-clickhouse.observability",
			"coroot-clickhouse-shard-0-0.coroot-clickhouse-headless.observability",
			"coroot-clickhouse.observability.svc.cluster.local",
			"telemetry.eu.example.com",
			"telemetry.eu.example.com.",
		} {
			mu.Lock()
			queries = nil
			mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if resolver.family != "" {
				addresses, lookupErr := (&net.Resolver{PreferGo: true}).LookupIP(ctx, resolver.family, name)
				if lookupErr != nil || len(addresses) != 1 || addresses[0].String() != "192.0.2.1" {
					cancel()
					return fmt.Errorf("go lookup %s: addresses=%v err=%v", name, addresses, lookupErr)
				}
			} else {
				// The isolated container has only loopback; do not let AI_ADDRCONFIG
				// suppress IPv4 before libc attempts the DNS lookup under test.
				output, lookupErr := exec.CommandContext(ctx, "getent", "--no-addrconfig", resolver.database, name).CombinedOutput()
				if lookupErr != nil || !strings.HasPrefix(string(output), "192.0.2.1") {
					cancel()
					return fmt.Errorf("libc lookup %s: output=%q err=%v", name, output, lookupErr)
				}
			}
			cancel()
			mu.Lock()
			observed := append([]string(nil), queries...)
			mu.Unlock()
			if len(observed) == 0 {
				return fmt.Errorf("%s lookup %s sent no DNS query", resolver.name, name)
			}
			for _, query := range observed {
				if !strings.HasSuffix(query, ".cluster.local.") && query != "telemetry.eu.example.com." {
					return fmt.Errorf("%s relative service lookup escaped cluster search domains: %s (queries=%v)", resolver.name, query, observed)
				}
			}
			absolute := strings.TrimSuffix(name, ".") + "."
			for _, query := range observed {
				if strings.Count(name, ".") >= 3 && query != absolute {
					return fmt.Errorf("%s expanded an absolute name: %v", resolver.name, observed)
				}
			}
			fmt.Printf("PASS %s %s: %v\n", resolver.name, name, observed)
		}
	}
	return nil
}
