/*
Copyright 2026 The Kaalm Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command mvprobe is the AgentTask the microVM e2e runs under Kata. It checks
// the guest from inside: the kernel it booted, the memory and CPUs it sees, a
// write and read on its task PVC, and the projected TLS files. It reports what it saw as artifacts over
// the gateway's mTLS completion endpoint, then waits for the controller to
// delete its Pod.
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// workspace is where the controller mounts a task's PVC when the task names
// no mountPath.
const workspace = "/var/task/workspace"

func main() {
	artifacts := map[string]string{
		"kernel":   kernel(),
		"memTotal": memTotal(),
		"cpus":     strconv.Itoa(runtime.NumCPU()),
		"memory":   memory(),
		"tls":      "unreadable",
	}
	client, err := mtlsClient()
	if err != nil {
		log.Printf("TLS files: %v", err)
	} else {
		artifacts["tls"] = "readable"
	}
	log.Printf("artifacts: %v", artifacts)
	if client != nil {
		report(client, artifacts)
	}
	select {}
}

func kernel() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "error: " + err.Error()
	}
	return strings.TrimSpace(string(b))
}

// memTotal is the MemTotal line of /proc/meminfo: the memory the guest
// kernel sees, which under Kata is the VM's size rather than the node's.
func memTotal() string {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return "error: " + err.Error()
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return "unknown"
}

// memory writes a file on the PVC and reads it back.
func memory() string {
	p := filepath.Join(workspace, "probe")
	if err := os.WriteFile(p, []byte("written"), 0o600); err != nil {
		return "error: " + err.Error()
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "error: " + err.Error()
	}
	return string(b)
}

func mtlsClient() (*http.Client, error) {
	cert, err := tls.LoadX509KeyPair(os.Getenv("KAALM_TLS_CERT"), os.Getenv("KAALM_TLS_KEY"))
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(os.Getenv("KAALM_CA_CERT"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS13,
		}},
	}, nil
}

// report posts the completion until the gateway accepts it. The gateway
// admits a task's report once its Pod informer has indexed the Pod's IP.
func report(client *http.Client, artifacts map[string]string) {
	body, _ := json.Marshal(map[string]any{
		"status":    "success",
		"message":   "microvm probe",
		"artifacts": artifacts,
	})
	url := os.Getenv("KAALM_GATEWAY_ENDPOINT") + "/v1/task/complete"
	for attempt := 1; attempt <= 60; attempt++ {
		resp, err := client.Post(url, "application/json", bytes.NewReader(body))
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				log.Printf("completion accepted (attempt %d)", attempt)
				return
			}
			log.Printf("completion answered %d (attempt %d)", resp.StatusCode, attempt)
		} else {
			log.Printf("completion failed (attempt %d): %v", attempt, err)
		}
		time.Sleep(2 * time.Second)
	}
}
