/*
Copyright 2026.

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

// Command worker is the sysctl applier/drift-checker daemon. It is run as the
// container image for both the applier DaemonSet (MODE=apply) and the
// drift-check CronJob (MODE=check) the operator creates.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sysctl-operator/internal/worker"
)

const (
	modeApply = "apply"
	modeCheck = "check"
)

func main() {
	mode := getenv("MODE", modeApply)
	configPath := getenv("CONFIG_PATH", "/etc/sysctl.d/99-sysctl-operator.conf")
	procRoot := getenv("PROC_ROOT", "/proc/sys")

	flag.StringVar(&mode, "mode", mode, "apply | check")
	flag.StringVar(&configPath, "config", configPath, "path to the mounted sysctl drop-in")
	flag.StringVar(&procRoot, "proc-root", procRoot, "root of the sysctl tree (override for tests)")
	flag.Parse()

	var (
		profile   = os.Getenv("PROFILE")
		namespace = os.Getenv("NAMESPACE")
		node      = os.Getenv("NODE_NAME")
		pod       = os.Getenv("POD_NAME")
		expected  = os.Getenv("CONFIG_HASH")
		reportURL = os.Getenv("REPORT_URL")
	)

	cfg, err := worker.Load(configPath)
	if err != nil {
		log.Fatalf("load config %q: %v", configPath, err)
	}

	// Hash check: refuse to act on a config that doesn't match what the
	// operator expects (e.g. a stale ConfigMap that hasn't propagated yet).
	if expected != "" && cfg.Hash != expected {
		log.Fatalf("config hash mismatch: mounted=%s expected=%s (stale ConfigMap?)", cfg.Hash, expected)
	}
	log.Printf("loaded %d sysctls hash=%s mode=%s", len(cfg.Entries), cfg.Hash, mode)

	rep := worker.Report{
		Profile:   profile,
		Namespace: namespace,
		Node:      node,
		Pod:       pod,
		Hash:      cfg.Hash,
	}

	switch mode {
	case modeApply:
		res := worker.Apply(procRoot, cfg.Entries)
		rep.Applied, rep.Failed = res.Applied, res.Failed
		rep.Success = res.OK()
		rep.Message = summarize("applied", res)
	case modeCheck:
		res := worker.Check(procRoot, cfg.Entries)
		rep.Applied = res.Applied
		rep.Failed = append(append([]string{}, res.Failed...), res.Drifted...)
		rep.Success = res.OK()
		rep.Message = summarize("checked", res)
	default:
		log.Fatalf("unknown mode %q (want %q or %q)", mode, modeApply, modeCheck)
	}

	sendReport(reportURL, rep)

	// In apply mode the process backs a DaemonSet, which expects a long-lived
	// container. Block until the operator tears the DaemonSet down (SIGTERM).
	// In check mode the process backs a one-shot CronJob, so we exit.
	if mode == modeApply {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		log.Printf("apply complete; waiting for shutdown signal")
		<-ctx.Done()
		log.Printf("shutting down")
	}
}

func sendReport(url string, rep worker.Report) {
	if url == "" {
		log.Printf("no REPORT_URL set; skipping report (success=%v, message=%q)", rep.Success, rep.Message)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := worker.NewReporter(url).Send(ctx, rep); err != nil {
		log.Printf("report to %s failed: %v", url, err)
		return
	}
	log.Printf("reported success=%v to %s", rep.Success, url)
}

func summarize(verb string, res worker.Result) string {
	return fmt.Sprintf("%s %d ok, %d failed, %d drifted",
		verb, len(res.Applied), len(res.Failed), len(res.Drifted))
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
