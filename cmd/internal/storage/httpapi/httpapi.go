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

// Package httpapi implements a cAdvisor storage driver that uploads collected
// container stats to a remote HTTP API endpoint as batched JSON payloads.
//
// Configuration is entirely via environment variables:
//
//	CADVISOR_METRICS_API_URL   – required; full URL to POST batches to
//	CADVISOR_METRICS_API_TOKEN – required; value used in "Authorization: Bearer <token>"
//
// Enable with: -storage_driver=httpapi
package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	info "github.com/google/cadvisor/info/v1"
	"github.com/google/cadvisor/storage"
	"github.com/google/cadvisor/version"

	"k8s.io/klog/v2"
)

func init() {
	storage.RegisterStorageDriver("httpapi", new)
}

// ---------------------------------------------------------------------------
// Wire types for the JSON payload
// ---------------------------------------------------------------------------

// batchEnvelope is the top-level object POSTed to the remote endpoint.
type batchEnvelope struct {
	SchemaVersion string       `json:"schema_version"`
	SentAt        time.Time    `json:"sent_at"`
	MachineName   string       `json:"machine_name"`
	Source        sourceInfo   `json:"source"`
	Samples       []sampleItem `json:"samples"`
}

type sourceInfo struct {
	Component string `json:"component"`
	Driver    string `json:"driver"`
	Version   string `json:"version,omitempty"`
}

type sampleItem struct {
	ContainerReference info.ContainerReference `json:"container_reference"`
	ContainerSpec      *info.ContainerSpec     `json:"container_spec,omitempty"`
	Stats              *info.ContainerStats    `json:"stats"`
}

// pendingSample is the internal representation enqueued on AddStats.
type pendingSample struct {
	ref   info.ContainerReference
	spec  *info.ContainerSpec // nil when caller did not supply Spec
	stats *info.ContainerStats
}

// ---------------------------------------------------------------------------
// Driver constants / tunables
// ---------------------------------------------------------------------------

const (
	schemaVersion = "1"
	driverName    = "httpapi"

	// maxBatchSize is the maximum number of samples that will be held in a
	// single pending batch before triggering an early flush.
	maxBatchSize = 500

	// queueCap is the capacity of the channel used to deliver samples from
	// AddStats to the background worker.  When the channel is full, new
	// samples are dropped with a logged warning.
	queueCap = 2000

	// httpTimeout is the per-request deadline for the POST to the remote API.
	httpTimeout = 30 * time.Second

	// maxSnippetLen is the maximum number of bytes of the response body that
	// are captured when the server returns a non-2xx status.
	maxSnippetLen = 256
)

// ---------------------------------------------------------------------------
// Driver struct
// ---------------------------------------------------------------------------

type httpAPIStorage struct {
	apiURL      string
	bearerToken string
	machineName string
	client      *http.Client
	userAgent   string

	// bufferDuration controls the timer-based flush interval.
	bufferDuration time.Duration

	// queue delivers samples from AddStats to the background goroutine.
	// It is a buffered channel; when full, samples are dropped.
	queue chan pendingSample

	// stopCh is closed by Close() to signal the background goroutine to exit.
	stopCh chan struct{}

	// doneCh is closed by the background goroutine once it has finished its
	// final flush, so Close() can block until the flush is complete.
	doneCh chan struct{}
}

// ---------------------------------------------------------------------------
// Constructor (registered as the factory)
// ---------------------------------------------------------------------------

func new() (storage.StorageDriver, error) {
	apiURL := os.Getenv("CADVISOR_METRICS_API_URL")
	if apiURL == "" {
		return nil, fmt.Errorf("httpapi storage driver: CADVISOR_METRICS_API_URL environment variable is required")
	}
	bearerToken := os.Getenv("CADVISOR_METRICS_API_TOKEN")
	if bearerToken == "" {
		return nil, fmt.Errorf("httpapi storage driver: CADVISOR_METRICS_API_TOKEN environment variable is required")
	}

	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("httpapi storage driver: failed to get hostname: %w", err)
	}

	// Build a reusable Transport/Client.  We disable automatic redirect
	// following so that misconfigured URLs surface as errors rather than
	// silently posting to the wrong destination.
	transport := &http.Transport{
		// Inherit reasonable defaults from http.DefaultTransport but keep
		// connection reuse so we do not open a new TCP connection per batch.
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	client := &http.Client{
		Timeout:   httpTimeout,
		Transport: transport,
		// Do not follow redirects: a 3xx almost certainly indicates a
		// misconfiguration, and silently reposting to a new URL is unsafe.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	cadvisorVersion := version.Info["version"]
	userAgent := fmt.Sprintf("cAdvisor/%s httpapi-storage", cadvisorVersion)

	bufferDuration := *storage.ArgDbBufferDuration

	s := &httpAPIStorage{
		apiURL:         apiURL,
		bearerToken:    bearerToken,
		machineName:    hostname,
		client:         client,
		userAgent:      userAgent,
		bufferDuration: bufferDuration,
		queue:          make(chan pendingSample, queueCap),
		stopCh:         make(chan struct{}),
		doneCh:         make(chan struct{}),
	}

	go s.worker()

	klog.V(1).Infof("httpapi storage driver: initialised (url=%s, buffer=%s)", apiURL, bufferDuration)
	return s, nil
}

// ---------------------------------------------------------------------------
// StorageDriver interface
// ---------------------------------------------------------------------------

// AddStats enqueues a snapshot of the stats for asynchronous upload.
// It returns immediately without performing any network I/O.
func (s *httpAPIStorage) AddStats(cInfo *info.ContainerInfo, stats *info.ContainerStats) error {
	if stats == nil {
		return nil
	}

	// Shallow-copy the ContainerReference so the caller's struct can be
	// mutated after this call returns without affecting our snapshot.
	refCopy := cInfo.ContainerReference

	// If the caller provided Spec metadata, capture it too.
	var specCopy *info.ContainerSpec
	if cInfo.Spec != (info.ContainerSpec{}) {
		sc := cInfo.Spec
		specCopy = &sc
	}

	// Stats is a large struct; shallow-copy it so buffers like PerCpu slices
	// are shared (they are effectively read-only in our context).
	statsCopy := *stats

	sample := pendingSample{
		ref:   refCopy,
		spec:  specCopy,
		stats: &statsCopy,
	}

	select {
	case s.queue <- sample:
		// delivered
	default:
		// Channel full – drop and log so operators know samples are being lost.
		klog.Warningf("httpapi storage driver: sample queue full (cap=%d); dropping sample for container %q", queueCap, cInfo.ContainerReference.Name)
	}
	return nil
}

// Close stops the background worker and synchronously flushes any remaining
// buffered samples before returning.
func (s *httpAPIStorage) Close() error {
	close(s.stopCh)
	<-s.doneCh // wait for the worker to finish its final flush
	s.client.CloseIdleConnections()
	klog.V(1).Infof("httpapi storage driver: closed")
	return nil
}

// ---------------------------------------------------------------------------
// Background worker
// ---------------------------------------------------------------------------

// worker runs in its own goroutine.  It collects samples from the queue and
// flushes them either on a timer or when the batch reaches maxBatchSize.
func (s *httpAPIStorage) worker() {
	defer close(s.doneCh)

	ticker := time.NewTicker(s.bufferDuration)
	defer ticker.Stop()

	batch := make([]pendingSample, 0, maxBatchSize)

	flush := func(reason string) {
		if len(batch) == 0 {
			return
		}
		toFlush := batch
		batch = make([]pendingSample, 0, maxBatchSize)
		if err := s.uploadBatch(toFlush); err != nil {
			klog.Errorf("httpapi storage driver: upload failed (%s): %v", reason, err)
		}
	}

	for {
		select {
		case <-s.stopCh:
			// Drain any remaining items already in the queue before final flush.
		drain:
			for {
				select {
				case sample := <-s.queue:
					batch = append(batch, sample)
				default:
					break drain
				}
			}
			flush("shutdown")
			return

		case sample := <-s.queue:
			batch = append(batch, sample)
			if len(batch) >= maxBatchSize {
				flush("batch-size-threshold")
			}

		case <-ticker.C:
			flush("timer")
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP upload
// ---------------------------------------------------------------------------

// uploadBatch serialises the given samples into a batchEnvelope and POSTs it
// to the configured API URL.
func (s *httpAPIStorage) uploadBatch(samples []pendingSample) error {
	items := make([]sampleItem, 0, len(samples))
	for _, ps := range samples {
		items = append(items, sampleItem{
			ContainerReference: ps.ref,
			ContainerSpec:      ps.spec,
			Stats:              ps.stats,
		})
	}

	envelope := batchEnvelope{
		SchemaVersion: schemaVersion,
		SentAt:        time.Now().UTC(),
		MachineName:   s.machineName,
		Source: sourceInfo{
			Component: "cadvisor",
			Driver:    driverName,
			Version:   version.Info["version"],
		},
		Samples: items,
	}

	body, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("json marshal failed: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, s.apiURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to build HTTP request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.bearerToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", s.userAgent)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP POST failed: %w", err)
	}
	// Always fully read and close the body so the connection can be reused.
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(maxSnippetLen+1)))
	if readErr != nil {
		// Non-fatal: we still have the status code to work with.
		klog.V(4).Infof("httpapi storage driver: error reading response body: %v", readErr)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(respBody)
		if len(snippet) > maxSnippetLen {
			snippet = snippet[:maxSnippetLen] + "…"
		}
		return fmt.Errorf("remote API returned HTTP %d; response: %s", resp.StatusCode, snippet)
	}

	klog.V(4).Infof("httpapi storage driver: uploaded %d samples, HTTP %d", len(samples), resp.StatusCode)
	return nil
}
