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

// Package metrics defines and registers the Prometheus metrics exported by the
// sysctl-operator. Metrics register into controller-runtime's global registry
// and are served on the manager's existing /metrics endpoint.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	labelProfile   = "profile"
	labelNamespace = "namespace"
	labelNode      = "node"
)

// ErroredPodsTotal counts applier pods that reported a failed apply, broken
// down by profile and node.
var ErroredPodsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "sysctl_operator_errored_pods_total",
		Help: "Total number of sysctl applier pods that reported an apply failure.",
	},
	[]string{labelProfile, labelNamespace, labelNode},
)

func init() {
	ctrlmetrics.Registry.MustRegister(ErroredPodsTotal)
}

// IncErrored records one errored applier pod for the given profile and node.
func IncErrored(profile, namespace, node string) {
	ErroredPodsTotal.WithLabelValues(profile, namespace, node).Inc()
}
