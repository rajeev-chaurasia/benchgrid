// bgctl submits experiments and waits for them, which is all CI needs. Its
// exit code is the gate: 0 SUCCEEDED, 1 FAILED or INVALID, 2 usage or
// transport errors, so a pipeline can tell a broken benchmark from a broken
// pipeline.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/rajeev-chaurasia/benchgrid/internal/server"
	"github.com/rajeev-chaurasia/benchgrid/internal/spec"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "submit":
		submit(os.Args[2:])
	case "get":
		get(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: bgctl submit -spec spec.json [-binary path] [-key k] [-wait]\n       bgctl get <experiment-id>")
	os.Exit(2)
}

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "bgctl: "+f+"\n", a...)
	os.Exit(2)
}

func submit(args []string) {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	control := fs.String("control", envOr("BENCHGRID_URL", "http://127.0.0.1:8080"), "control plane URL")
	specPath := fs.String("spec", "", "experiment spec JSON")
	binary := fs.String("binary", "", "upload this binary and set artifacts.binary_sha256 from it")
	key := fs.String("key", "", "idempotency key, so a retried CI step does not run twice")
	wait := fs.Bool("wait", false, "wait for a terminal state and exit by it")
	timeout := fs.Duration("timeout", 30*time.Minute, "how long -wait waits")
	fs.Parse(args)

	raw, err := os.ReadFile(*specPath)
	if err != nil {
		die("%v", err)
	}
	var sp spec.Spec
	if err := json.Unmarshal(raw, &sp); err != nil {
		die("spec: %v", err)
	}
	if *binary != "" {
		b, err := os.ReadFile(*binary)
		if err != nil {
			die("%v", err)
		}
		var out struct {
			SHA256 string `json:"sha256"`
		}
		if err := call(http.MethodPost, *control+"/v1/blobs", b, &out); err != nil {
			die("upload: %v", err)
		}
		sp.Artifacts.BinarySHA256 = out.SHA256
	}
	body, _ := json.Marshal(map[string]any{"spec": sp, "idempotency_key": *key})
	var e server.Experiment
	if err := call(http.MethodPost, *control+"/v1/experiments", body, &e); err != nil {
		die("submit: %v", err)
	}
	fmt.Println(e.ID)
	if !*wait {
		return
	}
	deadline := time.Now().Add(*timeout)
	for time.Now().Before(deadline) {
		if err := call(http.MethodGet, *control+"/v1/experiments/"+e.ID, nil, &e); err != nil {
			die("poll: %v", err)
		}
		switch e.State {
		case "SUCCEEDED":
			report(e)
			os.Exit(0)
		case "FAILED", "INVALID":
			report(e)
			os.Exit(1)
		}
		time.Sleep(time.Second)
	}
	die("timed out waiting for %s", e.ID)
}

func get(args []string) {
	if len(args) != 1 {
		usage()
	}
	var e server.Experiment
	if err := call(http.MethodGet, envOr("BENCHGRID_URL", "http://127.0.0.1:8080")+"/v1/experiments/"+args[0], nil, &e); err != nil {
		die("%v", err)
	}
	report(e)
}

func report(e server.Experiment) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(e)
}

func call(method, url string, body []byte, into any) error {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(b))
	}
	return json.Unmarshal(b, into)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
