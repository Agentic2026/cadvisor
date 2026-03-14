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

package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	info "github.com/google/cadvisor/info/v1"
	"github.com/google/cadvisor/storage"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setEnv is a test helper that sets and restores environment variables.
func setEnv(t *testing.T, url, token string) {
	t.Helper()
	t.Setenv(envURL, url)
	t.Setenv(envToken, token)
}

func TestNewDriverMissingURL(t *testing.T) {
	t.Setenv(envToken, "test-token")
	// Ensure URL is not set.
	t.Setenv(envURL, "")

	_, err := newDriver()
	require.Error(t, err)
	assert.Contains(t, err.Error(), envURL)
}

func TestNewDriverMissingToken(t *testing.T) {
	t.Setenv(envURL, "http://localhost:9999")
	// Ensure Token is not set.
	t.Setenv(envToken, "")

	_, err := newDriver()
	require.Error(t, err)
	assert.Contains(t, err.Error(), envToken)
}

func TestRegistered(t *testing.T) {
	drivers := storage.ListDrivers()
	found := false
	for _, d := range drivers {
		if d == "httpapi" {
			found = true
			break
		}
	}
	assert.True(t, found, "httpapi should be in registered drivers list: %v", drivers)
}

// newTestServer returns a test HTTP server and channels/state for inspecting
// received requests.
type testServer struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []*receivedRequest
}

type receivedRequest struct {
	Method      string
	ContentType string
	Auth        string
	UserAgent   string
	Body        []byte
	Payload     payload
}

func newTestServer(t *testing.T, statusCode int) *testServer {
	t.Helper()
	ts := &testServer{}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		defer r.Body.Close()

		var p payload
		_ = json.Unmarshal(body, &p) // best-effort parse

		ts.mu.Lock()
		ts.requests = append(ts.requests, &receivedRequest{
			Method:      r.Method,
			ContentType: r.Header.Get("Content-Type"),
			Auth:        r.Header.Get("Authorization"),
			UserAgent:   r.Header.Get("User-Agent"),
			Body:        body,
			Payload:     p,
		})
		ts.mu.Unlock()

		w.WriteHeader(statusCode)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(ts.server.Close)
	return ts
}

func (ts *testServer) getRequests() []*receivedRequest {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	result := make([]*receivedRequest, len(ts.requests))
	copy(result, ts.requests)
	return result
}

func makeTestCInfo() *info.ContainerInfo {
	return &info.ContainerInfo{
		ContainerReference: info.ContainerReference{
			Id:        "abc123",
			Name:      "/docker/abc123",
			Aliases:   []string{"my-container"},
			Namespace: "docker",
		},
		Spec: info.ContainerSpec{
			CreationTime: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			Labels:       map[string]string{"app": "test"},
			Image:        "nginx:latest",
		},
	}
}

func makeTestStats(ts time.Time) *info.ContainerStats {
	return &info.ContainerStats{
		Timestamp: ts,
		Cpu: info.CpuStats{
			Usage: info.CpuUsage{
				Total:  100000,
				System: 50000,
				User:   50000,
			},
		},
		Memory: info.MemoryStats{
			Usage:      1024 * 1024,
			WorkingSet: 512 * 1024,
		},
	}
}

// newTestDriver creates a driver pointed at the test server with a short buffer duration.
func newTestDriver(t *testing.T, serverURL string) *httpAPIStorage {
	t.Helper()
	setEnv(t, serverURL, "test-secret-token")
	d, err := newDriver()
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	return d.(*httpAPIStorage)
}

func TestSendsPOSTWithCorrectHeaders(t *testing.T) {
	ts := newTestServer(t, http.StatusOK)
	d := newTestDriver(t, ts.server.URL)

	cInfo := makeTestCInfo()
	stats := makeTestStats(time.Now())
	require.NoError(t, d.AddStats(cInfo, stats))

	// Trigger immediate flush.
	require.NoError(t, d.flushBatch())

	reqs := ts.getRequests()
	require.Len(t, reqs, 1)

	req := reqs[0]
	assert.Equal(t, http.MethodPost, req.Method)
	assert.Equal(t, "application/json", req.ContentType)
	assert.Equal(t, "Bearer test-secret-token", req.Auth)
	assert.Contains(t, req.UserAgent, "httpapi-storage")
}

func TestPayloadContainsValidJSON(t *testing.T) {
	ts := newTestServer(t, http.StatusOK)
	d := newTestDriver(t, ts.server.URL)

	cInfo := makeTestCInfo()
	sampleTime := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	stats := makeTestStats(sampleTime)
	require.NoError(t, d.AddStats(cInfo, stats))
	require.NoError(t, d.flushBatch())

	reqs := ts.getRequests()
	require.Len(t, reqs, 1)

	p := reqs[0].Payload
	assert.Equal(t, schemaVersion, p.SchemaVersion)
	assert.NotEmpty(t, p.MachineName)
	assert.Equal(t, "cadvisor", p.Source.Component)
	assert.Equal(t, "httpapi", p.Source.Driver)
	require.Len(t, p.Samples, 1)

	sample := p.Samples[0]
	assert.Equal(t, "/docker/abc123", sample.ContainerReference.Name)
	assert.Equal(t, "abc123", sample.ContainerReference.Id)
	assert.NotNil(t, sample.Stats)
	assert.Equal(t, sampleTime, sample.Stats.Timestamp)
	assert.Equal(t, uint64(100000), sample.Stats.Cpu.Usage.Total)
}

func TestPayloadContainsContainerSpec(t *testing.T) {
	ts := newTestServer(t, http.StatusOK)
	d := newTestDriver(t, ts.server.URL)

	cInfo := makeTestCInfo()
	stats := makeTestStats(time.Now())
	require.NoError(t, d.AddStats(cInfo, stats))
	require.NoError(t, d.flushBatch())

	reqs := ts.getRequests()
	require.Len(t, reqs, 1)

	sample := reqs[0].Payload.Samples[0]
	require.NotNil(t, sample.ContainerSpec, "container_spec must be present")
	assert.Equal(t, "nginx:latest", sample.ContainerSpec.Image)
	assert.Equal(t, map[string]string{"app": "test"}, sample.ContainerSpec.Labels)
}

func TestFlushOnBatchThreshold(t *testing.T) {
	ts := newTestServer(t, http.StatusOK)
	setEnv(t, ts.server.URL, "test-token")
	d, err := newDriver()
	require.NoError(t, err)
	defer d.Close()
	driver := d.(*httpAPIStorage)

	cInfo := makeTestCInfo()
	for i := 0; i < maxBatchSize; i++ {
		require.NoError(t, driver.AddStats(cInfo, makeTestStats(time.Now().Add(time.Duration(i)*time.Second))))
	}

	// Wait for the background goroutine to pick up the flushCh signal.
	time.Sleep(500 * time.Millisecond)

	reqs := ts.getRequests()
	require.GreaterOrEqual(t, len(reqs), 1, "should have flushed on reaching batch threshold")

	totalSamples := 0
	for _, r := range reqs {
		totalSamples += len(r.Payload.Samples)
	}
	assert.Equal(t, maxBatchSize, totalSamples)
}

func TestNon2xxResponseProducesError(t *testing.T) {
	ts := newTestServer(t, http.StatusInternalServerError)
	d := newTestDriver(t, ts.server.URL)

	cInfo := makeTestCInfo()
	require.NoError(t, d.AddStats(cInfo, makeTestStats(time.Now())))

	err := d.flushBatch()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestCloseFlushesRemaining(t *testing.T) {
	ts := newTestServer(t, http.StatusOK)
	setEnv(t, ts.server.URL, "test-token")
	d, err := newDriver()
	require.NoError(t, err)

	cInfo := makeTestCInfo()
	for i := 0; i < 5; i++ {
		require.NoError(t, d.AddStats(cInfo, makeTestStats(time.Now().Add(time.Duration(i)*time.Second))))
	}

	// Close should flush pending samples.
	require.NoError(t, d.Close())

	reqs := ts.getRequests()
	totalSamples := 0
	for _, r := range reqs {
		totalSamples += len(r.Payload.Samples)
	}
	assert.Equal(t, 5, totalSamples, "Close() must flush all pending samples")
}

func TestResponseBodyIsConsumed(t *testing.T) {
	// Verify that the driver reads the response body. We check this
	// indirectly by ensuring that multiple sequential requests succeed
	// (HTTP connection reuse requires body consumption).
	ts := newTestServer(t, http.StatusOK)
	d := newTestDriver(t, ts.server.URL)

	cInfo := makeTestCInfo()
	for round := 0; round < 3; round++ {
		require.NoError(t, d.AddStats(cInfo, makeTestStats(time.Now())))
		require.NoError(t, d.flushBatch())
	}

	reqs := ts.getRequests()
	assert.Len(t, reqs, 3, "all 3 sequential requests should succeed")
}

func TestAddStatsNilStats(t *testing.T) {
	ts := newTestServer(t, http.StatusOK)
	d := newTestDriver(t, ts.server.URL)

	cInfo := makeTestCInfo()
	assert.NoError(t, d.AddStats(cInfo, nil))

	assert.NoError(t, d.flushBatch())
	assert.Empty(t, ts.getRequests(), "nil stats should not generate a request")
}

func TestDropsWhenQueueFull(t *testing.T) {
	// Construct the driver struct directly (without the background goroutine)
	// to test the queue-full dropping logic without concurrent flushing.
	d := &httpAPIStorage{
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
		flushCh: make(chan struct{}, 1),
	}

	cInfo := makeTestCInfo()
	// Fill the queue beyond maxPendingSamples.
	for i := 0; i < maxPendingSamples+100; i++ {
		_ = d.AddStats(cInfo, makeTestStats(time.Now()))
	}

	d.mu.Lock()
	dropped := d.dropped
	pending := d.pending
	d.mu.Unlock()

	assert.Equal(t, uint64(100), dropped, "should have dropped 100 samples")
	assert.Equal(t, maxPendingSamples, pending, "pending should be capped at maxPendingSamples")

	// Clean up: stop the channels (no goroutine to wait for).
	close(d.doneCh)
	close(d.stopCh)
}

func TestFlushOnTimer(t *testing.T) {
	// Override the buffer duration to something very short for the test.
	origDuration := *storage.ArgDbBufferDuration
	*storage.ArgDbBufferDuration = 200 * time.Millisecond
	defer func() { *storage.ArgDbBufferDuration = origDuration }()

	ts := newTestServer(t, http.StatusOK)
	setEnv(t, ts.server.URL, "test-token")
	d, err := newDriver()
	require.NoError(t, err)
	defer d.Close()

	cInfo := makeTestCInfo()
	require.NoError(t, d.AddStats(cInfo, makeTestStats(time.Now())))

	// Wait for the timer to fire.
	time.Sleep(600 * time.Millisecond)

	reqs := ts.getRequests()
	require.GreaterOrEqual(t, len(reqs), 1, "timer should have triggered a flush")
}
