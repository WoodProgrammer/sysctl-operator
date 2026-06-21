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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Report is the payload POSTed to the operator's /api/v1/reports endpoint. Its
// JSON shape must match the operator's server.StatusReport.
type Report struct {
	Profile   string   `json:"profile"`
	Namespace string   `json:"namespace"`
	Node      string   `json:"node"`
	Pod       string   `json:"pod,omitempty"`
	Hash      string   `json:"hash,omitempty"`
	Success   bool     `json:"success"`
	Applied   []string `json:"applied,omitempty"`
	Failed    []string `json:"failed,omitempty"`
	Message   string   `json:"message,omitempty"`
}

// Reporter sends reports to the operator API.
type Reporter struct {
	URL    string
	Client *http.Client
}

// NewReporter returns a Reporter with a sane default timeout.
func NewReporter(url string) *Reporter {
	return &Reporter{URL: url, Client: &http.Client{Timeout: 10 * time.Second}}
}

// Send POSTs the report, returning an error on transport or non-200 responses.
func (r *Reporter) Send(ctx context.Context, rep Report) error {
	body, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("report rejected: status %d", resp.StatusCode)
	}
	return nil
}
