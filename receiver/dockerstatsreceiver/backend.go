// Copyright 2020 OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dockerstatsreceiver

import (
	"context"
	"time"

	dEvents "github.com/docker/docker/api/types/events"
)

type containerBase struct {
	ID    string
	Image string
}

type container struct {
	ID       string
	Image    string
	Name     string
	Hostname string
	Labels   map[string]string

	StateRunning bool
	StatePaused  bool
	// Env          []string

	// other
	EnvMap map[string]string
}

type blkioStatEntry struct {
	Major uint64
	Minor uint64
	Op    string
	Value uint64
}

type blkioStats struct {
	IoServiceBytesRecursive []blkioStatEntry
	IoServicedRecursive     []blkioStatEntry
	IoQueuedRecursive       []blkioStatEntry
	IoServiceTimeRecursive  []blkioStatEntry
	IoWaitTimeRecursive     []blkioStatEntry
	IoMergedRecursive       []blkioStatEntry
	IoTimeRecursive         []blkioStatEntry
	SectorsRecursive        []blkioStatEntry
}

type cpuUsage struct {
	// Total CPU time consumed.
	// Units: nanoseconds (Linux)
	// Units: 100's of nanoseconds (Windows)
	TotalUsage        uint64
	PercpuUsage       []uint64
	UsageInKernelmode uint64
	UsageInUsermode   uint64
}

type throttlingData struct {
	Periods          uint64
	ThrottledPeriods uint64
	ThrottledTime    uint64
}

type cpuStats struct {
	OnlineCPUs     uint32
	SystemUsage    uint64
	CPUUsage       cpuUsage
	ThrottlingData throttlingData
}

type memoryStats struct {
	Usage    uint64
	MaxUsage uint64
	Stats    map[string]uint64
	Limit    uint64
}

type networkStats struct {
	RxBytes    uint64
	RxPackets  uint64
	RxErrors   uint64
	RxDropped  uint64
	TxBytes    uint64
	TxPackets  uint64
	TxErrors   uint64
	TxDropped  uint64
	EndpointID string
	InstanceID string
}

type stats struct {
	ID      string
	Name    string
	Read    time.Time
	PreRead time.Time

	BlkioStats  blkioStats
	CPUStats    cpuStats
	PreCPUStats cpuStats
	MemoryStats memoryStats

	Networks map[string]networkStats
}

/*
type event struct {
	ID       string `json:"id,omitempty"`
	Action   string
	TimeNano int64 `json:"timeNano,omitempty"`
}
*/

type eventFilterOption struct {
	Key, Value string
}

type filterOptions struct {
	status string
}

type backend interface {
	List(ct context.Context, filters filterOptions) ([]containerBase, error)
	Inspect(ct context.Context, id string) (*container, error)
	Stats(ctx context.Context, id string) (stats, error)
	Events(ctx context.Context, since time.Time, filters []eventFilterOption) (<-chan dEvents.Message, <-chan error)
}

type errDoesNotExist struct {
	err error
}

func (e errDoesNotExist) Error() string {
	return e.err.Error()
}

func (e errDoesNotExist) Unwrap() error {
	return e.err
}
