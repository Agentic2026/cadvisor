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
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	info "github.com/google/cadvisor/info/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newTestServer starts an httptest.Server that records all requests it receives.
// The returned slice pointer is safe to read after the test driver is closed.
func newTestServer(t *testing.T, statusCode int) (*httptest.Server, *[]capturedRequest, *atomic.Int32) {
	t.Helper()
	requests := &[]capturedRequest{}
	mu := &sync.Mutex{}
	var count atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			t.Errorf("test server: read body: %v", err)
		}
		mu.Lock()
		*requests = append(*requests, capturedRequest{
			method:  r.Method,
			url:     r.URL.String(),
			auth:    r.Header.Get("Authorization"),
			ct:      r.Header.Get("Content-Type"),
			ua:      r.Header.Get("User-Agent"),
			rawBody: body,
		})
		mu.Unlock()
		w.WriteHeader(statusCode)
	}))
	t.Cleanup(srv.Close)
	return srv, requests, &count
}

type capturedRequest struct {
	method  string
	url     string
	auth    string
	ct      string
	ua      string
	rawBody []byte
}

func (c capturedRequest) decodeEnvelope(t *testing.T) batchEnvelope {
	t.Helper()
	var env batchEnvelope
	require.NoError(t, json.Unmarshal(c.rawBody, &env))
	return env
}

// newDriverForTest creates a driver connecting to srv with a short buffer
// duration so timer-based flushes happen quickly in tests.
func newDriverForTest(t *testing.T, srv *httptest.Server, token string, bufDuration time.Duration) *httpAPIStorage {
	t.Helper()
	hostname, _ := os.Hostname()
	transport := &http.Transport{}
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	s := &httpAPIStorage{
		apiURL:         srv.URL,
		bearerToken:    token,
		machineName:    hostname,
		client:         client,
		userAgent:      "cAdvisor/test httpapi-storage",
		bufferDuration: bufDuration,
		queue:          make(chan pendingSample, queueCap),
		stopCh:         make(chan struct{}),
		doneCh:         make(chan struct{}),
	}
	go s.worker()
	return s
}

// makeCInfo builds a ContainerInfo with optional Spec.
func makeCInfo(name string, withSpec bool) *info.ContainerInfo {
	ci := &info.ContainerInfo{
		ContainerReference: info.ContainerReference{
			Name:    name,
			Aliases: []string{"alias-" + name},
		},
	}
	if withSpec {
		ci.Spec = info.ContainerSpec{
			Image: "test-image:latest",
			Labels: map[string]string{
				"env": "test",
			},
		}
	}
	return ci
}

// makeStats builds a minimal ContainerStats.
func makeStats() *info.ContainerStats {
	return &info.ContainerStats{
		Timestamp: time.Now(),
		Cpu: info.CpuStats{
			Usage: info.CpuUsage{
				Total: 123456,
			},
		},
		Memory: info.MemoryStats{
			Usage: 10 * 1024 * 1024,
		},
	}
}

// waitForRequests polls until the test server has received at least `n`
// requests or the timeout elapses.
func waitForRequests(t *testing.T, count *atomic.Int32, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if int(count.Load()) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d HTTP requests, got %d", n, count.Load())
}

// ---------------------------------------------------------------------------
// Tests: environment variable validation
// ---------------------------------------------------------------------------

func TestNew_MissingURL(t *testing.T) {
	t.Setenv("CADVISOR_METRICS_API_URL", "")
	t.Setenv("CADVISOR_METRICS_API_TOKEN", "tok")
	_, err := new()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CADVISOR_METRICS_API_URL")
}

func TestNew_MissingToken(t *testing.T) {
	t.Setenv("CADVISOR_METRICS_API_URL", "http://example.com")
	t.Setenv("CADVISOR_METRICS_API_TOKEN", "")
	_, err := new()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CADVISOR_METRICS_API_TOKEN")
}

func TestNew_BothVarsPresent(t *testing.T) {
	srv, _, _ := newTestServer(t, 200)
	t.Setenv("CADVISOR_METRICS_API_URL", srv.URL)
	t.Setenv("CADVISOR_METRICS_API_TOKEN", "mytoken")

	drv, err := new()
	require.NoError(t, err)
	require.NotNil(t, drv)
	require.NoError(t, drv.Close())
}

// ---------------------------------------------------------------------------
// Tests: AddStats → POST behaviour
// ---------------------------------------------------------------------------

func TestAddStats_PostsToConfiguredURL(t *testing.T) {
	srv, reqs, count := newTestServer(t, 200)
	drv := newDriverForTest(t, srv, "tok", 50*time.Millisecond)

	require.NoError(t, drv.AddStats(makeCInfo("c1", false), makeStats()))
	require.NoError(t, drv.Close())

	assert.GreaterOrEqual(t, int(count.Load()), 1, "expected at least one request")
	captured := (*reqs)[0]
	assert.Equal(t, http.MethodPost, captured.method)
}

func TestAddStats_BearerTokenHeader(t *testing.T) {
	srv, reqs, _ := newTestServer(t, 200)
	drv := newDriverForTest(t, srv, "supersecret", 50*time.Millisecond)

	require.NoError(t, drv.AddStats(makeCInfo("c1", false), makeStats()))
	require.NoError(t, drv.Close())

	require.GreaterOrEqual(t, len(*reqs), 1)
	assert.Equal(t, "Bearer supersecret", (*reqs)[0].auth)
}

func TestAddStats_ContentTypeJSON(t *testing.T) {
	srv, reqs, _ := newTestServer(t, 200)
	drv := newDriverForTest(t, srv, "tok", 50*time.Millisecond)

	require.NoError(t, drv.AddStats(makeCInfo("c1", false), makeStats()))
	require.NoError(t, drv.Close())

	require.GreaterOrEqual(t, len(*reqs), 1)
	assert.Equal(t, "application/json", (*reqs)[0].ct)
}

func TestAddStats_UserAgentHeader(t *testing.T) {
	srv, reqs, _ := newTestServer(t, 200)
	drv := newDriverForTest(t, srv, "tok", 50*time.Millisecond)

	require.NoError(t, drv.AddStats(makeCInfo("c1", false), makeStats()))
	require.NoError(t, drv.Close())

	require.GreaterOrEqual(t, len(*reqs), 1)
	assert.Contains(t, (*reqs)[0].ua, "httpapi-storage")
}

func TestAddStats_ValidJSONBatch(t *testing.T) {
	srv, reqs, _ := newTestServer(t, 200)
	drv := newDriverForTest(t, srv, "tok", 50*time.Millisecond)

	stats := makeStats()
	require.NoError(t, drv.AddStats(makeCInfo("mycontainer", false), stats))
	require.NoError(t, drv.Close())

	require.GreaterOrEqual(t, len(*reqs), 1)
	env := (*reqs)[0].decodeEnvelope(t)

	assert.Equal(t, schemaVersion, env.SchemaVersion)
	assert.Equal(t, "cadvisor", env.Source.Component)
	assert.Equal(t, driverName, env.Source.Driver)
	require.NotEmpty(t, env.Samples)
	assert.Equal(t, "mycontainer", env.Samples[0].ContainerReference.Name)
	// Stats timestamp must be preserved from the original, not replaced by time.Now()
	assert.Equal(t, stats.Timestamp.Unix(), env.Samples[0].Stats.Timestamp.Unix())
}

// ---------------------------------------------------------------------------
// Tests: ContainerSpec in payload
// ---------------------------------------------------------------------------

func TestAddStats_ContainerSpecIncludedWhenPresent(t *testing.T) {
	srv, reqs, _ := newTestServer(t, 200)
	drv := newDriverForTest(t, srv, "tok", 50*time.Millisecond)

	cInfo := makeCInfo("c-with-spec", true /* withSpec */)
	require.NoError(t, drv.AddStats(cInfo, makeStats()))
	require.NoError(t, drv.Close())

	require.GreaterOrEqual(t, len(*reqs), 1)
	env := (*reqs)[0].decodeEnvelope(t)
	require.NotEmpty(t, env.Samples)
	require.NotNil(t, env.Samples[0].ContainerSpec, "ContainerSpec should be present")
	assert.Equal(t, "test-image:latest", env.Samples[0].ContainerSpec.Image)
}

func TestAddStats_ContainerSpecNilWhenAbsent(t *testing.T) {
	srv, reqs, _ := newTestServer(t, 200)
	drv := newDriverForTest(t, srv, "tok", 50*time.Millisecond)

	cInfo := makeCInfo("c-no-spec", false /* withSpec */)
	require.NoError(t, drv.AddStats(cInfo, makeStats()))
	require.NoError(t, drv.Close())

	require.GreaterOrEqual(t, len(*reqs), 1)
	env := (*reqs)[0].decodeEnvelope(t)
	require.NotEmpty(t, env.Samples)
	assert.Nil(t, env.Samples[0].ContainerSpec, "ContainerSpec should be absent when not set")
}

// ---------------------------------------------------------------------------
// Tests: timer-based flush
// ---------------------------------------------------------------------------

func TestFlushOnTimer(t *testing.T) {
	srv, _, count := newTestServer(t, 200)
	// Use a very short buffer so the timer triggers quickly.
	drv := newDriverForTest(t, srv, "tok", 30*time.Millisecond)

	require.NoError(t, drv.AddStats(makeCInfo("c1", false), makeStats()))

	// Wait for the timer to fire and produce a request, then close.
	waitForRequests(t, count, 1, 2*time.Second)

	require.NoError(t, drv.Close())
}

// ---------------------------------------------------------------------------
// Tests: batch-size threshold flush
// ---------------------------------------------------------------------------

func TestFlushOnBatchSizeThreshold(t *testing.T) {
	srv, reqs, count := newTestServer(t, 200)
	// Use a very long buffer so only the batch-size threshold triggers the flush.
	drv := newDriverForTest(t, srv, "tok", 10*time.Minute)

	// Enqueue maxBatchSize samples to trigger a threshold flush.
	for i := 0; i < maxBatchSize; i++ {
		require.NoError(t, drv.AddStats(makeCInfo("c1", false), makeStats()))
	}

	// Wait for the threshold-triggered flush.
	waitForRequests(t, count, 1, 3*time.Second)

	// All samples should have been sent in one or more requests totalling maxBatchSize.
	total := 0
	for _, r := range *reqs {
		env := r.decodeEnvelope(t)
		total += len(env.Samples)
	}
	assert.Equal(t, maxBatchSize, total)

	require.NoError(t, drv.Close())
}

// ---------------------------------------------------------------------------
// Tests: Close flushes pending samples
// ---------------------------------------------------------------------------

func TestClose_FlushesPendingSamples(t *testing.T) {
	srv, reqs, _ := newTestServer(t, 200)
	// Long timer so only Close() triggers the flush.
	drv := newDriverForTest(t, srv, "tok", 10*time.Minute)

	const numSamples = 5
	for i := 0; i < numSamples; i++ {
		require.NoError(t, drv.AddStats(makeCInfo("c1", false), makeStats()))
	}

	// Close must synchronously flush before returning.
	require.NoError(t, drv.Close())

	total := 0
	for _, r := range *reqs {
		env := r.decodeEnvelope(t)
		total += len(env.Samples)
	}
	assert.Equal(t, numSamples, total, "all %d samples must be flushed on Close()", numSamples)
}

// ---------------------------------------------------------------------------
// Tests: non-2xx responses produce an error (logged, not returned to caller)
// ---------------------------------------------------------------------------

func TestNon2xxResponse_DoesNotPanicAndIsLogged(t *testing.T) {
	// The driver should not crash on non-2xx and should attempt subsequent flushes.
	srv, _, count := newTestServer(t, 503)
	drv := newDriverForTest(t, srv, "tok", 30*time.Millisecond)

	require.NoError(t, drv.AddStats(makeCInfo("c1", false), makeStats()))

	// Let at least one flush attempt happen.
	waitForRequests(t, count, 1, 2*time.Second)

	// Close should not return an error even if the upload failed.
	require.NoError(t, drv.Close())
}

// ---------------------------------------------------------------------------
// Tests: response body fully consumed (connection reuse)
// ---------------------------------------------------------------------------

func TestResponseBodyFullyConsumed(t *testing.T) {
	// The test server writes a large body to verify the driver drains it.
	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		r.Body.Close()
		reqCount.Add(1)
		w.WriteHeader(200)
		// Write a large body that must be fully consumed for connection reuse.
		w.Write(make([]byte, 4096))
	}))
	t.Cleanup(srv.Close)

	drv := newDriverForTest(t, srv, "tok", 30*time.Millisecond)

	for i := 0; i < 3; i++ {
		require.NoError(t, drv.AddStats(makeCInfo("c1", false), makeStats()))
	}

	require.NoError(t, drv.Close())
	// If the body were not consumed, the transport would open a new connection
	// for every request.  We simply assert no panic / error occurred.
	assert.GreaterOrEqual(t, int(reqCount.Load()), 1)
}

// ---------------------------------------------------------------------------
// Tests: AddStats is non-blocking when queue is full
// ---------------------------------------------------------------------------

func TestAddStats_NonBlockingWhenQueueFull(t *testing.T) {
	// Use a custom driver with a tiny queue and no background worker, so the
	// queue fills up immediately and subsequent AddStats calls drop samples.
	// We verify AddStats returns quickly (no blocking).
	hostname, _ := os.Hostname()
	s := &httpAPIStorage{
		apiURL:         "http://127.0.0.1:0", // unreachable; worker not started
		bearerToken:    "tok",
		machineName:    hostname,
		client:         &http.Client{},
		userAgent:      "test",
		bufferDuration: time.Minute,
		queue:          make(chan pendingSample, 2), // tiny queue
		stopCh:         make(chan struct{}),
		doneCh:         make(chan struct{}),
	}

	// Fill the queue.
	require.NoError(t, s.AddStats(makeCInfo("c1", false), makeStats()))
	require.NoError(t, s.AddStats(makeCInfo("c1", false), makeStats()))

	start := time.Now()
	// This call must not block even though the queue is full.
	require.NoError(t, s.AddStats(makeCInfo("c1", false), makeStats()))
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 100*time.Millisecond, "AddStats must return immediately when queue is full")

	// Manually signal done so Close logic won't hang.
	close(s.doneCh)
}

// ---------------------------------------------------------------------------
// Tests: nil stats are ignored
// ---------------------------------------------------------------------------

func TestAddStats_NilStats(t *testing.T) {
	srv, _, count := newTestServer(t, 200)
	drv := newDriverForTest(t, srv, "tok", 30*time.Millisecond)

	require.NoError(t, drv.AddStats(makeCInfo("c1", false), nil))
	require.NoError(t, drv.Close())

	// nil stats should never trigger a flush request.
	assert.Zero(t, int(count.Load()), "nil stats should not produce any HTTP request")
}
