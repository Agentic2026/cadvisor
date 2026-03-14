// Copyright 2024 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package httpapi implements a cAdvisor storage driver that uploads
// collected container metrics to a remote HTTP API endpoint in batched
// JSON payloads.  It is activated via -storage_driver=httpapi and
// configured exclusively through environment variables.
package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	info "github.com/google/cadvisor/info/v1"
	"github.com/google/cadvisor/storage"
	"github.com/google/cadvisor/version"

	"k8s.io/klog/v2"
)

const (
	// Environment variable names.
	envURL   = "CADVISOR_METRICS_API_URL"
	envToken = "CADVISOR_METRICS_API_TOKEN"

	// maxBatchSize is the maximum number of samples that will be batched
	// together into a single POST request.
	maxBatchSize = 100

	// maxPendingSamples bounds memory usage.  When the internal queue
	// reaches this size, new samples are dropped with a warning.
	maxPendingSamples = 10000

	// schemaVersion is embedded in every outbound payload so the
	// receiving API can version-gate deserialization.
	schemaVersion = 1

	// HTTP client timeouts.
	httpTimeout          = 30 * time.Second
	httpIdleTimeout      = 90 * time.Second
	httpIdleConns        = 2
	httpTLSHandshakeTO   = 10 * time.Second
	httpResponseHeaderTO = 10 * time.Second
)

func init() {
	storage.RegisterStorageDriver("httpapi", newDriver)
}

// --- payload types -----------------------------------------------------------

type payload struct {
	SchemaVersion int           `json:"schema_version"`
	SentAt        time.Time     `json:"sent_at"`
	MachineName   string        `json:"machine_name"`
	Source        payloadSource `json:"source"`
	Samples       []sampleEntry `json:"samples"`
}

type payloadSource struct {
	Component string `json:"component"`
	Driver    string `json:"driver"`
	Version   string `json:"version,omitempty"`
}

type sampleEntry struct {
	ContainerReference info.ContainerReference `json:"container_reference"`
	ContainerSpec      *info.ContainerSpec     `json:"container_spec,omitempty"`
	Stats              *info.ContainerStats    `json:"stats"`
}

// --- driver ------------------------------------------------------------------

type httpAPIStorage struct {
	url         string
	token       string
	machineName string
	client      *http.Client
	userAgent   string

	mu       sync.Mutex
	batch    []sampleEntry
	pending  int // total samples queued (batch)
	dropped  uint64
	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
	flushCh  chan struct{} // signal immediate flush when batch is full
}

func newDriver() (storage.StorageDriver, error) {
	apiURL := os.Getenv(envURL)
	if apiURL == "" {
		return nil, fmt.Errorf("httpapi storage driver: %s environment variable is required", envURL)
	}
	apiToken := os.Getenv(envToken)
	if apiToken == "" {
		return nil, fmt.Errorf("httpapi storage driver: %s environment variable is required", envToken)
	}

	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("httpapi storage driver: failed to get hostname: %w", err)
	}

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   httpTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          httpIdleConns,
		MaxIdleConnsPerHost:   httpIdleConns,
		IdleConnTimeout:       httpIdleTimeout,
		TLSHandshakeTimeout:   httpTLSHandshakeTO,
		ResponseHeaderTimeout: httpResponseHeaderTO,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   httpTimeout,
		// Do not follow redirects automatically.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	ver := version.Version
	if ver == "" {
		ver = "dev"
	}

	d := &httpAPIStorage{
		url:         apiURL,
		token:       apiToken,
		machineName: hostname,
		client:      client,
		userAgent:   fmt.Sprintf("cAdvisor/%s httpapi-storage", ver),
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
		flushCh:     make(chan struct{}, 1),
	}

	go d.loop()
	return d, nil
}

// AddStats enqueues a sample snapshot for asynchronous upload.
// It never performs blocking network I/O.
func (d *httpAPIStorage) AddStats(cInfo *info.ContainerInfo, stats *info.ContainerStats) error {
	if stats == nil {
		return nil
	}

	entry := sampleEntry{
		ContainerReference: cInfo.ContainerReference,
		Stats:              stats,
	}
	// Include Spec only when it looks populated (non-zero CreationTime or
	// labels present).  This keeps payloads lean for root cgroups that
	// never have meaningful spec data.
	if !cInfo.Spec.CreationTime.IsZero() || len(cInfo.Spec.Labels) > 0 || cInfo.Spec.Image != "" {
		spec := cInfo.Spec
		entry.ContainerSpec = &spec
	}

	d.mu.Lock()
	if d.pending >= maxPendingSamples {
		d.dropped++
		if d.dropped%1000 == 1 {
			klog.Warningf("httpapi storage driver: dropping sample (total dropped: %d); queue full (%d pending)", d.dropped, d.pending)
		}
		d.mu.Unlock()
		return nil
	}
	d.batch = append(d.batch, entry)
	d.pending++
	shouldFlush := d.pending >= maxBatchSize
	d.mu.Unlock()

	if shouldFlush {
		select {
		case d.flushCh <- struct{}{}:
		default:
		}
	}
	return nil
}

// Close stops the background worker, synchronously flushes remaining
// buffered samples, and closes idle HTTP connections.
func (d *httpAPIStorage) Close() error {
	var err error
	d.stopOnce.Do(func() {
		close(d.stopCh)
		<-d.doneCh // wait for the background goroutine to exit

		// Final synchronous flush of anything remaining.
		err = d.flushBatch()

		// Release idle connections.
		d.client.CloseIdleConnections()
	})
	return err
}

// --- background loop ---------------------------------------------------------

func (d *httpAPIStorage) loop() {
	defer close(d.doneCh)

	bufferDuration := *storage.ArgDbBufferDuration
	if bufferDuration <= 0 {
		bufferDuration = 60 * time.Second
	}
	ticker := time.NewTicker(bufferDuration)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopCh:
			return
		case <-ticker.C:
			if err := d.flushBatch(); err != nil {
				klog.Errorf("httpapi storage driver: flush error: %v", err)
			}
		case <-d.flushCh:
			if err := d.flushBatch(); err != nil {
				klog.Errorf("httpapi storage driver: flush error: %v", err)
			}
		}
	}
}

// flushBatch sends all currently buffered samples as one or more
// HTTP POST requests (in chunks of maxBatchSize).
func (d *httpAPIStorage) flushBatch() error {
	d.mu.Lock()
	if len(d.batch) == 0 {
		d.mu.Unlock()
		return nil
	}
	samples := d.batch
	d.batch = nil
	d.pending = 0
	d.mu.Unlock()

	// Send in chunks of maxBatchSize.
	for i := 0; i < len(samples); i += maxBatchSize {
		end := i + maxBatchSize
		if end > len(samples) {
			end = len(samples)
		}
		if err := d.send(samples[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func (d *httpAPIStorage) send(samples []sampleEntry) error {
	p := payload{
		SchemaVersion: schemaVersion,
		SentAt:        time.Now().UTC(),
		MachineName:   d.machineName,
		Source: payloadSource{
			Component: "cadvisor",
			Driver:    "httpapi",
			Version:   version.Version,
		},
		Samples: samples,
	}

	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("httpapi storage driver: marshal error: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("httpapi storage driver: request creation error: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("User-Agent", d.userAgent)

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("httpapi storage driver: POST error: %w", err)
	}
	// Always fully read and close the response body so the underlying
	// TCP connection can be reused by the transport's connection pool.
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(respBody)
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		return fmt.Errorf("httpapi storage driver: non-2xx response %d: %s", resp.StatusCode, snippet)
	}
	return nil
}
