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

package worker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ifacePlaceholder marks where a network interface name is substituted.
const ifacePlaceholder = "{iface}"

// Result captures the outcome of an apply or check run.
type Result struct {
	// Applied lists the concrete keys applied (apply) or in-spec (check).
	Applied []string
	// Failed lists keys that could not be written/read, with an error detail.
	Failed []string
	// Drifted lists keys whose live value differs from desired (check only).
	Drifted []string
}

// OK reports whether the run had no failures or drift.
func (r Result) OK() bool { return len(r.Failed) == 0 && len(r.Drifted) == 0 }

// Apply sets each entry via "sysctl -w key=value", expanding any "{iface}"
// placeholder across the matching network interfaces. root is used only to
// enumerate interfaces for expansion; the write itself goes through the sysctl
// binary so it targets the live kernel exactly like a manual "sysctl -w".
func Apply(root string, entries []Entry) Result {
	var res Result
	for _, e := range entries {
		for _, key := range expand(root, e.Name) {
			if err := writeSysctl(key, e.Value); err != nil {
				res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", key, err))
				continue
			}
			res.Applied = append(res.Applied, key)
		}
	}
	return res
}

// Check reads each entry's live value and records drift, without writing.
func Check(root string, entries []Entry) Result {
	var res Result
	for _, e := range entries {
		for _, key := range expand(root, e.Name) {
			cur, err := readSysctl(root, key)
			if err != nil {
				res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", key, err))
				continue
			}
			if cur != e.Value {
				res.Drifted = append(res.Drifted, fmt.Sprintf("%s=%s want=%s", key, cur, e.Value))
				continue
			}
			res.Applied = append(res.Applied, key)
		}
	}
	return res
}

// writeSysctl applies a single key via "sysctl -w key=value". The binary can be
// overridden with SYSCTL_BIN (defaults to "sysctl" on $PATH).
func writeSysctl(key, value string) error {
	out, err := exec.Command(sysctlBinary(), "-w", fmt.Sprintf("%s=%s", key, value)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sysctl -w %s=%s: %v: %s", key, value, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sysctlBinary returns the sysctl executable to invoke.
func sysctlBinary() string {
	if b := os.Getenv("SYSCTL_BIN"); b != "" {
		return b
	}
	return "sysctl"
}

func readSysctl(root, key string) (string, error) {
	data, err := os.ReadFile(keyToPath(root, key))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// keyToPath converts a dotted sysctl key to its /proc/sys file path.
func keyToPath(root, key string) string {
	return filepath.Join(root, filepath.Clean(strings.ReplaceAll(key, ".", "/")))
}

// expand returns the concrete keys for a name. A name without the placeholder
// returns itself; a name with "{iface}" is expanded across the interfaces found
// under its conf directory (skipping the aggregate "all"/"default" and "lo").
//
// TODO: the interfaceSelector (prefixes/exclude) from the CRD is not yet
// available here because the ConfigMap only carries "name = value". A later
// step should render a richer config so prefixes/exclude are honored.
func expand(root, name string) []string {
	idx := strings.Index(name, ifacePlaceholder)
	if idx < 0 {
		return []string{name}
	}
	prefix := strings.TrimRight(name[:idx], ".")
	suffix := strings.TrimLeft(name[idx+len(ifacePlaceholder):], ".")

	entries, err := os.ReadDir(keyToPath(root, prefix))
	if err != nil {
		return nil
	}
	var keys []string
	for _, d := range entries {
		iface := d.Name()
		if iface == "all" || iface == "default" || iface == "lo" {
			continue
		}
		keys = append(keys, prefix+"."+iface+"."+suffix)
	}
	return keys
}
