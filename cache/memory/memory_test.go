// Copyright 2014 Google Inc. All Rights Reserved.
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

package memory

import (
	"fmt"
	"testing"
	"time"

	info "github.com/google/cadvisor/info/v1"
	"github.com/google/cadvisor/storage"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const containerName = "/container"

var (
	cInfo = info.ContainerInfo{
		ContainerReference: info.ContainerReference{Name: containerName},
	}
	zero time.Time
)

// Make stats with the specified identifier.
func makeStat(i int) *info.ContainerStats {
	return &info.ContainerStats{
		Timestamp: zero.Add(time.Duration(i) * time.Second),
		Cpu: info.CpuStats{
			LoadAverage: int32(i),
		},
	}
}

func getRecentStats(t *testing.T, memoryCache *InMemoryCache, numStats int) []*info.ContainerStats {
	stats, err := memoryCache.RecentStats(containerName, zero, zero, numStats)
	require.Nil(t, err)
	return stats
}

func TestAddStats(t *testing.T) {
	memoryCache := New(60*time.Second, nil)

	assert := assert.New(t)
	assert.Nil(memoryCache.AddStats(&cInfo, makeStat(0)))
	assert.Nil(memoryCache.AddStats(&cInfo, makeStat(1)))
	assert.Nil(memoryCache.AddStats(&cInfo, makeStat(2)))
	assert.Nil(memoryCache.AddStats(&cInfo, makeStat(0)))
	cInfo2 := info.ContainerInfo{
		ContainerReference: info.ContainerReference{
			Name: "/container2",
		},
	}
	assert.Nil(memoryCache.AddStats(&cInfo2, makeStat(0)))
	assert.Nil(memoryCache.AddStats(&cInfo2, makeStat(1)))
}

func TestRecentStatsNoRecentStats(t *testing.T) {
	memoryCache := makeWithStats(t, 0)

	_, err := memoryCache.RecentStats(containerName, zero, zero, 60)
	assert.NotNil(t, err)
}

// Make an instance of InMemoryCache with n stats.
func makeWithStats(t *testing.T, n int) *InMemoryCache {
	memoryCache := New(60*time.Second, nil)

	for i := 0; i < n; i++ {
		assert.NoError(t, memoryCache.AddStats(&cInfo, makeStat(i)))
	}
	return memoryCache
}

func TestRecentStatsGetZeroStats(t *testing.T) {
	memoryCache := makeWithStats(t, 10)

	assert.Len(t, getRecentStats(t, memoryCache, 0), 0)
}

func TestRecentStatsGetSomeStats(t *testing.T) {
	memoryCache := makeWithStats(t, 10)

	assert.Len(t, getRecentStats(t, memoryCache, 5), 5)
}

func TestRecentStatsGetAllStats(t *testing.T) {
	memoryCache := makeWithStats(t, 10)

	assert.Len(t, getRecentStats(t, memoryCache, -1), 10)
}

// mockStorageDriver is a minimal storage driver used to verify that
// InMemoryCache.Close() closes all backend drivers.
type mockStorageDriver struct {
	closed   bool
	closeErr error
	addCalls int
}

func (m *mockStorageDriver) AddStats(_ *info.ContainerInfo, _ *info.ContainerStats) error {
	m.addCalls++
	return nil
}

func (m *mockStorageDriver) Close() error {
	m.closed = true
	return m.closeErr
}

func TestCloseClosesBackendDrivers(t *testing.T) {
	backend1 := &mockStorageDriver{}
	backend2 := &mockStorageDriver{}
	memoryCache := New(60*time.Second, []storage.StorageDriver{backend1, backend2})

	// Add some stats first so there's state.
	assert.NoError(t, memoryCache.AddStats(&cInfo, makeStat(0)))

	err := memoryCache.Close()
	assert.NoError(t, err)
	assert.True(t, backend1.closed, "backend1 should be closed")
	assert.True(t, backend2.closed, "backend2 should be closed")
}

func TestCloseHandlesBackendErrors(t *testing.T) {
	backend1 := &mockStorageDriver{closeErr: fmt.Errorf("backend close failed")}
	backend2 := &mockStorageDriver{}
	memoryCache := New(60*time.Second, []storage.StorageDriver{backend1, backend2})

	err := memoryCache.Close()
	assert.Error(t, err, "should report error from backend1")
	assert.Contains(t, err.Error(), "backend close failed")
	// Both backends should still be closed even if one errors.
	assert.True(t, backend1.closed, "backend1 should be closed")
	assert.True(t, backend2.closed, "backend2 should be closed even if backend1 errored")
}
