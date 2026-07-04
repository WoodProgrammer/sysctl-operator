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
	"strings"
)

// scriptHeaderPrefix and scriptFooter delimit each script block in the rendered
// file. They must match the controller's renderScripts.
const (
	scriptHeaderPrefix = "#!script "
	scriptFooter       = "#!endscript"
)

// ScriptEntry is a single custom script parsed from the rendered file.
type ScriptEntry struct {
	Name        string
	Interpreter string
	Content     string
}

// ScriptConfig is the parsed scripts file plus the hash of its raw bytes.
type ScriptConfig struct {
	// Raw is the exact file content (the bytes the operator hashed).
	Raw string
	// Hash matches the operator's hashContent: sha256 of Raw, first 16 hex.
	Hash string
	// Scripts are the parsed script blocks, in file order.
	Scripts []ScriptEntry
}

// LoadScripts reads and parses the rendered scripts file at path.
func LoadScripts(path string) (*ScriptConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseScripts(data), nil
}

// ParseScripts parses the blocks emitted by the controller's renderScripts:
//
//	#!script name=<name> interpreter=<interpreter>
//	<content lines...>
//	#!endscript
func ParseScripts(data []byte) *ScriptConfig {
	c := &ScriptConfig{Raw: string(data), Hash: HashBytes(data)}
	var (
		cur     *ScriptEntry
		content []string
	)
	for _, line := range strings.Split(c.Raw, "\n") {
		switch {
		case strings.HasPrefix(line, scriptHeaderPrefix):
			name, interp := parseScriptHeader(line)
			cur = &ScriptEntry{Name: name, Interpreter: interp}
			content = content[:0]
		case line == scriptFooter:
			if cur != nil {
				cur.Content = strings.Join(content, "\n")
				c.Scripts = append(c.Scripts, *cur)
				cur = nil
			}
		default:
			if cur != nil {
				content = append(content, line)
			}
		}
	}
	return c
}

// parseScriptHeader extracts name and interpreter from a header line of the form
// "#!script name=<name> interpreter=<interpreter>".
func parseScriptHeader(line string) (name, interpreter string) {
	interpreter = "/bin/sh"
	for _, field := range strings.Fields(strings.TrimPrefix(line, scriptHeaderPrefix)) {
		key, val, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch key {
		case "name":
			name = val
		case "interpreter":
			interpreter = val
		}
	}
	return name, interpreter
}

// RunScripts executes each script by piping its content into the configured
// interpreter's stdin. A script that exits non-zero (or fails to start) is
// recorded in Failed; the rest still run.
func RunScripts(scripts []ScriptEntry) Result {
	var res Result
	for _, s := range scripts {
		interp := s.Interpreter
		if interp == "" {
			interp = "/bin/sh"
		}
		cmd := exec.Command(interp)
		cmd.Stdin = strings.NewReader(s.Content)
		if out, err := cmd.CombinedOutput(); err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s: %v: %s", s.Name, err, strings.TrimSpace(string(out))))
			continue
		}
		res.Applied = append(res.Applied, s.Name)
	}
	return res
}
