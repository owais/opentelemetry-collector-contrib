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
	"encoding/json"
	"fmt"
	"io"
	"time"

	dtypes "github.com/docker/docker/api/types"
	dEvents "github.com/docker/docker/api/types/events"
	dfilters "github.com/docker/docker/api/types/filters"
	docker "github.com/docker/docker/client"
	"go.uber.org/zap"
)

const (
	dockerAPIVersion = "v1.22"
	userAgent        = "OpenTelemetry-Collector Docker Stats Receiver/v0.0.1"
)

type dockerBackend struct {
	client *docker.Client
	logger *zap.Logger
}

func newDockerBackend(endpoint string, logger *zap.Logger) (*dockerBackend, error) {
	client, err := docker.NewClientWithOpts(
		docker.WithHost(endpoint),
		docker.WithVersion(dockerAPIVersion),
		docker.WithHTTPHeaders(map[string]string{"User-Agent": userAgent}),
	)
	if err != nil {
		return nil, fmt.Errorf("could not create docker client: %w", err)
	}

	return &dockerBackend{client, logger}, nil

}

func (d *dockerBackend) Events(ctx context.Context, since time.Time, fops []eventFilterOption) (<-chan dEvents.Event, <-chan error) {
	filters := []dfilters.KeyValuePair{}
	for _, f := range fops {
		filters = append(filters, dfilters.KeyValuePair{
			Key:   f.Key,
			Value: f.Value,
		})
	}

	options := dtypes.EventsOptions{
		Filters: dfilters.NewArgs(filters...),
		Since:   since.Format(time.RFC3339Nano),
	}
	return d.client.Events(ctx, options)
}

func (d *dockerBackend) List(ctx context.Context, fops filterOptions) ([]containerBase, error) {
	filters := dfilters.NewArgs()
	filters.Add("status", fops.status)
	options := dtypes.ContainerListOptions{
		Filters: filters,
	}

	dockerContainers, err := d.client.ContainerList(ctx, options)
	if err != nil {
		return nil, err
	}

	containers := make([]containerBase, len(dockerContainers))
	for _, dc := range dockerContainers {
		containers = append(containers, containerBase{
			ID: dc.ID,
		})
	}

	return containers, nil
}

func (d *dockerBackend) Inspect(ctx context.Context, id string) (*container, error) {
	c, err := d.client.ContainerInspect(ctx, id)
	if err != nil {
		return nil, err
	}
	return &container{
		ID:           c.ID,
		Image:        c.Image,
		Name:         c.Name,
		Hostname:     c.Config.Hostname,
		Labels:       c.Config.Labels,
		StateRunning: c.State.Running,
		StatePaused:  c.State.Paused,
		EnvMap:       containerEnvToMap(c.Config.Env),
	}, nil
}

func (d *dockerBackend) translateStats(s *dtypes.StatsJSON) *stats {
	networks := make(map[string]networkStats, len(s.Networks))
	for k, v := range s.Networks {
		networks[k] = networkStats(v)
	}

	return &stats{
		ID:      s.ID,
		Name:    s.Name,
		Read:    s.Read,
		PreRead: s.PreRead,
		BlkioStats: blkioStats{
			IoServiceBytesRecursive: []blkioStatEntry(s.BlkioStats.IoServiceBytesRecursive),
			/*
				IoServicedRecursive:
				IoQueuedRecursive:
				IoServiceTimeRecursive:
				IoWaitTimeRecursive     []blkioStatEntry
				IoMergedRecursive       []blkioStatEntry
				IoTimeRecursive         []blkioStatEntry
				SectorsRecursive        []blkioStatEntry
			*/
		},
		CPUStats: cpuStats{
			OnlineCPUs:     s.CPUStats.OnlineCPUs,
			SystemUsage:    s.CPUStats.SystemUsage,
			CPUUsage:       cpuUsage(s.CPUStats.CPUUsage),
			ThrottlingData: throttlingData(s.CPUStats.ThrottlingData),
		},
		PreCPUStats: cpuStats{
			OnlineCPUs:     s.PreCPUStats.OnlineCPUs,
			SystemUsage:    s.PreCPUStats.SystemUsage,
			CPUUsage:       cpuUsage(s.PreCPUStats.CPUUsage),
			ThrottlingData: throttlingData(s.PreCPUStats.ThrottlingData),
		},
		MemoryStats: memoryStats{
			Usage:    s.MemoryStats.Usage,
			MaxUsage: s.MemoryStats.MaxUsage,
			Stats:    s.MemoryStats.Stats,
			Limit:    s.MemoryStats.Limit,
		},
		Networks: networks,
	}
}

func (d *dockerBackend) Stats(ctx context.Context, id string) (*stats, error) {
	containerStats, err := d.client.ContainerStats(ctx, id, false)
	if err != nil {
		if docker.IsErrNotFound(err) {
			err = errDoesNotExist{err}
		}
		return nil, err
	}

	statsJSON, err := d.toStatsJSON(containerStats, id)
	if err != nil {
		return nil, err
	}
	_ = statsJSON
	// TODO: convert statsJSON to stats
	return d.translateStats(statsJSON), nil
}

func (d *dockerBackend) toStatsJSON(
	containerStats dtypes.ContainerStats,
	id string,
) (*dtypes.StatsJSON, error) {
	var statsJSON dtypes.StatsJSON
	err := json.NewDecoder(containerStats.Body).Decode(&statsJSON)
	containerStats.Body.Close()
	if err != nil {
		// EOF means there aren't any containerStats, perhaps because the container has been removed.
		if err == io.EOF {
			// It isn't indicative of actual error.
			return nil, err
		}
		d.logger.Error(
			"Could not parse docker containerStats for container id",
			zap.String("id", id),
			zap.Error(err),
		)
		return nil, err
	}
	return &statsJSON, nil
}
