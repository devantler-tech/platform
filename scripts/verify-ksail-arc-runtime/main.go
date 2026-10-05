package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "verify-image" {
		flags := flag.NewFlagSet("verify-image", flag.ContinueOnError)
		digest := flags.String("digest", "", "signed image descriptor")
		err := flags.Parse(os.Args[2:])
		var selected string
		if err == nil && flags.NArg() == 0 {
			var data []byte
			data, err = io.ReadAll(io.LimitReader(os.Stdin, (4<<20)+1))
			if err == nil {
				selected, err = runtimeDigest(data, *digest)
			}
		}
		if err != nil || selected == "" {
			fmt.Fprintln(os.Stderr, "ARC acceptance: immutable image proof failed")
			os.Exit(1)
		}
		fmt.Println(selected)
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ARC acceptance: correlated denial proof failed")
		os.Exit(1)
	}
	fmt.Println("PASS: ARC runtime evidence verified")
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "verify-budget" {
		flags := flag.NewFlagSet("verify-budget", flag.ContinueOnError)
		node := flags.String("node", "", "private node readback")
		reservations := flags.String("reservations", "", "filtered resource requests")
		uid := flags.String("probe-uid", "", "owned probe UID")
		if err := flags.Parse(os.Args[2:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *node == "" || *reservations == "" || *uid == "" {
			return fmt.Errorf("missing capacity readback")
		}
		data, err := os.ReadFile(*node)
		if err != nil {
			return err
		}
		requests, err := os.ReadFile(*reservations)
		if err != nil {
			return err
		}
		return verifyBudget(data, requests, *uid)
	}
	if len(os.Args) > 1 && os.Args[1] == "verify-quota" {
		if len(os.Args) != 2 {
			return fmt.Errorf("invalid quota arguments")
		}
		data, err := io.ReadAll(io.LimitReader(os.Stdin, (4<<20)+1))
		if err != nil || len(data) > 4<<20 {
			return fmt.Errorf("invalid quota evidence")
		}
		return verifyQuota(data)
	}
	if len(os.Args) > 1 && os.Args[1] == "verify-pod" {
		flags := flag.NewFlagSet("verify-pod", flag.ContinueOnError)
		desired := flags.String("desired", "", "private desired Pod file")
		actual := flags.String("actual", "", "private admitted Pod file")
		image := flags.String("runtime-image", "", "verified platform image")
		if err := flags.Parse(os.Args[2:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *desired == "" || *actual == "" {
			return fmt.Errorf("missing pod readback")
		}
		want, err := os.ReadFile(*desired)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(*actual)
		if err != nil {
			return err
		}
		return verifyPod(want, got, *image)
	}
	pod := flag.String("pod", "", "exact owned probe name")
	source := flag.String("source", "", "pinned probe address")
	destination := flag.String("destination", "", "healthy target address")
	destinationPod := flag.String("destination-pod", "", "optional pinned target pod")
	destinationNamespace := flag.String("destination-namespace", "", "optional pinned target namespace")
	node := flag.String("node", "", "exact Hubble observer identity")
	port := flag.Uint("port", 0, "target TCP port")
	since := flag.String("since", "", "attempt start")
	until := flag.String("until", "", "attempt end")
	flag.Parse()
	start, err := time.Parse(time.RFC3339Nano, *since)
	if err != nil {
		return err
	}
	end, err := time.Parse(time.RFC3339Nano, *until)
	if err != nil {
		return err
	}
	if flag.NArg() != 0 || *pod == "" || *node == "" ||
		net.ParseIP(*source) == nil || net.ParseIP(*destination) == nil ||
		*port == 0 || *port > 65535 || end.Before(start) || end.Sub(start) > 30*time.Second {
		return fmt.Errorf("invalid bounded target")
	}
	return verifyDenial(os.Stdin, denialTarget{
		namespace: "arc-ksail-analysis", pod: *pod, source: *source, destination: *destination,
		destinationNamespace: *destinationNamespace, destinationPod: *destinationPod,
		node: *node, port: uint16(*port), since: start, until: end,
	})
}
