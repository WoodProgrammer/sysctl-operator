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

// Package worker implements the sysctl applier/drift-checker daemon. It reads
// the sysctl drop-in mounted from the operator's ConfigMap, verifies its hash,
// applies (or audits) the values against /proc/sys, and reports the result to
// the operator's API.
package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
)

// Entry is a single sysctl key/value parsed from the drop-in file.
type Entry struct {
	Name  string
	Value string
}

// Config is the parsed sysctl drop-in plus the hash of its raw bytes.
type Config struct {
	// Raw is the exact file content (the bytes the operator hashed).
	Raw string
	// Hash matches the operator's hashContent: sha256 of Raw, first 16 hex.
	Hash string
	// Entries are the parsed key/value pairs, in file order.
	Entries []Entry
}

// Load reads and parses the drop-in file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data), nil
}

// Parse parses drop-in content (lines of "name = value", '#'/';' comments).
func Parse(data []byte) *Config {
	c := &Config{Raw: string(data), Hash: HashBytes(data)}
	for _, line := range strings.Split(c.Raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		c.Entries = append(c.Entries, Entry{
			Name:  strings.TrimSpace(key),
			Value: strings.TrimSpace(val),
		})
	}
	return c
}

// HashBytes computes the same hash the operator stores (sha256, first 16 hex).
// Keeping this identical to the controller's hashContent is what lets the
// worker verify it received the exact config the operator intended.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}
