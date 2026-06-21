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

// Package server implements the operator's HTTP API. Applier pods POST their
// per-node sysctl application status to this endpoint; the handler records the
// result on the owning SysctlProfile's status.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sysctlv1alpha1 "sysctl-operator/api/v1alpha1"
	"sysctl-operator/internal/metrics"
)

// StatusReport is the payload an applier pod POSTs to report the outcome of
// applying a profile's sysctls on a single node.
type StatusReport struct {
	// Profile is the SysctlProfile name.
	Profile string `json:"profile"`
	// Namespace is the SysctlProfile namespace.
	Namespace string `json:"namespace"`
	// Node is the node the applier pod ran on.
	Node string `json:"node"`
	// Pod is the reporting pod name (optional, for diagnostics).
	Pod string `json:"pod,omitempty"`
	// Hash is the config hash the pod applied (optional).
	Hash string `json:"hash,omitempty"`
	// Success indicates whether all sysctls applied cleanly.
	Success bool `json:"success"`
	// Applied lists the keys successfully applied (optional).
	Applied []string `json:"applied,omitempty"`
	// Failed lists the keys that failed to apply (optional).
	Failed []string `json:"failed,omitempty"`
	// Message carries a human-readable detail (optional).
	Message string `json:"message,omitempty"`
}

// ReportServer is a manager.Runnable that serves the operator's report API.
type ReportServer struct {
	Client client.Client
	// Addr is the listen address, e.g. ":9090".
	Addr string
}

// NeedLeaderElection ensures the API serves on every replica, not just the
// elected leader, so any pod can reach a reachable endpoint.
func (s *ReportServer) NeedLeaderElection() bool { return false }

// Start runs the HTTP server until the context is cancelled.
func (s *ReportServer) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("report-server")

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v1/reports", s.handleReport)

	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Shut the server down gracefully when the manager stops.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error(err, "report server graceful shutdown failed")
		}
	}()

	log.Info("starting report server", "addr", s.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *ReportServer) handleReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logf.FromContext(ctx).WithName("report-server")

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var report StatusReport
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		http.Error(w, "invalid payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if report.Profile == "" || report.Namespace == "" || report.Node == "" {
		http.Error(w, "profile, namespace and node are required", http.StatusBadRequest)
		return
	}

	if err := s.applyReport(ctx, &report); err != nil {
		// If the profile is gone, this is a stale report from a pod that
		// outlived its profile — acknowledge so the pod stops retrying.
		if client.IgnoreNotFound(err) == nil {
			log.Info("report for unknown profile, ignoring",
				"profile", report.Profile, "namespace", report.Namespace, "node", report.Node)
			w.WriteHeader(http.StatusOK)
			return
		}
		log.Error(err, "failed to record report", "profile", report.Profile, "node", report.Node)
		http.Error(w, "failed to record report", http.StatusInternalServerError)
		return
	}

	// Record the Prometheus metric once, after the status update succeeded, so
	// retry-on-conflict cannot double-count.
	if !report.Success {
		metrics.IncErrored(report.Profile, report.Namespace, report.Node)
	}

	log.Info("recorded report", "profile", report.Profile, "node", report.Node, "success", report.Success)
	w.WriteHeader(http.StatusOK)
}

// applyReport records the report on the profile's status, retrying on conflict.
func (s *ReportServer) applyReport(ctx context.Context, report *StatusReport) error {
	key := types.NamespacedName{Name: report.Profile, Namespace: report.Namespace}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var profile sysctlv1alpha1.SysctlProfile
		if err := s.Client.Get(ctx, key, &profile); err != nil {
			return err
		}

		phase := sysctlv1alpha1.NodePhaseApplied
		if !report.Success {
			phase = sysctlv1alpha1.NodePhaseFailed
		}

		upsertNodeStatus(&profile, report, phase)
		recomputeCounts(&profile)

		return s.Client.Status().Update(ctx, &profile)
	})
}

// upsertNodeStatus inserts or updates the per-node entry for the report.
func upsertNodeStatus(profile *sysctlv1alpha1.SysctlProfile, report *StatusReport, phase sysctlv1alpha1.NodePhase) {
	now := metav1.Now()
	for i := range profile.Status.NodeStatuses {
		ns := &profile.Status.NodeStatuses[i]
		if ns.NodeName != report.Node {
			continue
		}
		if !report.Success {
			ns.FailCount++
			profile.Status.ErroredPods++
		} else {
			ns.FailCount = 0
			ns.AppliedHash = report.Hash
		}
		ns.Phase = phase
		ns.Message = report.Message
		ns.LastTransitionTime = now
		return
	}

	ns := sysctlv1alpha1.NodeStatus{
		NodeName:           report.Node,
		Phase:              phase,
		Message:            report.Message,
		LastTransitionTime: now,
	}
	if report.Success {
		ns.AppliedHash = report.Hash
	} else {
		ns.FailCount = 1
		profile.Status.ErroredPods++
	}
	profile.Status.NodeStatuses = append(profile.Status.NodeStatuses, ns)
}

// recomputeCounts refreshes the aggregate node counters from per-node status.
func recomputeCounts(profile *sysctlv1alpha1.SysctlProfile) {
	var applied, failed int32
	for _, ns := range profile.Status.NodeStatuses {
		switch ns.Phase {
		case sysctlv1alpha1.NodePhaseApplied:
			applied++
		case sysctlv1alpha1.NodePhaseFailed:
			failed++
		}
	}
	profile.Status.AppliedNodes = applied
	profile.Status.FailedNodes = failed
}
