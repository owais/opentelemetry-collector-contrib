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
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	agentmetricspb "github.com/census-instrumentation/opencensus-proto/gen-go/agent/metrics/v1"
	"go.uber.org/zap"
)

// dockerClient provides the core metric gathering functionality from the Docker Daemon.
// It retrieves container information in two forms to produce metric data: dtypes.ContainerJSON
// from client.ContainerInspect() for container information (id, name, hostname, labels, and env)
// and dtypes.StatsJSON from client.ContainerStats() for metric values.
type dockerClient struct {
	// client               *docker.Client
	backend backend
	config  *Config
	// containersOld        map[string]DockerContainer
	containers           map[string]*container
	containersLock       sync.Mutex
	excludedImageMatcher *StringMatcher
	logger               *zap.Logger
}

func newDockerClient(config *Config, logger *zap.Logger) (*dockerClient, error) {
	backend, err := newDockerBackend(config.Endpoint, logger)
	if err != nil {
		return nil, err
	}

	excludedImageMatcher, err := NewStringMatcher(config.ExcludedImages)
	if err != nil {
		return nil, fmt.Errorf("could not determine docker client excluded images: %w", err)
	}

	dc := &dockerClient{
		backend:              backend,
		config:               config,
		logger:               logger,
		containers:           make(map[string]*container),
		containersLock:       sync.Mutex{},
		excludedImageMatcher: excludedImageMatcher,
	}

	return dc, nil
}

// Provides a slice of DockerContainers to use for individual FetchContainerStats calls.
func (dc *dockerClient) Containers() []container {
	dc.containersLock.Lock()
	defer dc.containersLock.Unlock()
	containers := make([]container, 0, len(dc.containers))
	for _, container := range dc.containers {
		containers = append(containers, *container)
	}
	return containers
}

// LoadContainerList will load the initial running container maps for
// inspection and establishing which containers warrant stat gathering calls
// by the receiver.
func (dc *dockerClient) LoadContainerList(ctx context.Context) error {
	// Build initial container maps before starting loop
	listCtx, cancel := context.WithTimeout(ctx, dc.config.Timeout)
	/*
		filters := dfilters.NewArgs()
		filters.Add("status", "running")
		options := dtypes.ContainerListOptions{
			Filters: filters,
		}
		listCtx, cancel := context.WithTimeout(ctx, dc.config.Timeout)
		containerList, err := dc.client.ContainerList(listCtx, options)
	*/
	containerList, err := dc.backend.List(listCtx, filterOptions{status: "running"})
	defer cancel()
	if err != nil {
		return err
	}

	wg := sync.WaitGroup{}
	for _, c := range containerList {
		wg.Add(1)
		go func(container container) {
			if !dc.shouldBeExcluded(container.Image) {
				if cnt, ok := dc.inspectedContainerIsOfInterest(ctx, container.ID); ok {
					dc.persistContainer(cnt)
				}
			} else {
				dc.logger.Debug(
					"Not monitoring container per ExcludedImages",
					zap.String("image", container.Image),
					zap.String("id", container.ID),
				)
			}
			wg.Done()
		}(c)
	}
	wg.Wait()
	return nil
}

// FetchContainerStatsAndConvertToMetrics will query the desired container stats and send
// converted metrics to the results channel, since this is intended to be run in a goroutine.
func (dc *dockerClient) FetchContainerStatsAndConvertToMetrics(
	ctx context.Context,
	container container,
) (*agentmetricspb.ExportMetricsServiceRequest, error) {
	dc.logger.Debug("Fetching container stats.", zap.String("id", container.ID))
	statsCtx, cancel := context.WithTimeout(ctx, dc.config.Timeout)
	stats, err := dc.backend.Stats(statsCtx, container.ID)
	// TODO: check if err indicates container does not exist and should be removed from map.
	// needs a new custom error that should wrap internally

	defer cancel()
	if err != nil {
		if errors.As(err, &errDoesNotExist{}) {
			dc.logger.Debug(
				"Daemon reported container doesn't exist. Will no longer monitor.",
				zap.String("id", container.ID),
			)
			dc.removeContainer(container.ID)
		} else {
			dc.logger.Warn(
				"Could not fetch docker containerStats for container",
				zap.String("id", container.ID),
				zap.Error(err),
			)
		}

		return nil, err
	}

	md, err := ContainerStatsToMetrics(&stats, &container, dc.config)
	if err != nil {
		dc.logger.Error(
			"Could not convert docker containerStats for container id",
			zap.String("id", container.ID),
			zap.Error(err),
		)
		return nil, err
	}
	return md, nil
}

func (dc *dockerClient) ContainerEventLoop(ctx context.Context) {
	filters := []eventFilterOption{
		{Key: "type", Value: "container"},
		{Key: "event", Value: "destroy"},
		{Key: "event", Value: "die"},
		{Key: "event", Value: "pause"},
		{Key: "event", Value: "stop"},
		{Key: "event", Value: "start"},
		{Key: "event", Value: "unpause"},
		{Key: "event", Value: "update"},
	}
	lastTime := time.Now()

EVENT_LOOP:
	for {
		// eventCh, errCh := dc.client.Events(ctx, options)
		eventCh, errCh := dc.backend.Events(ctx, lastTime, filters)

		for {
			select {
			case <-ctx.Done():
				return
			case event := <-eventCh:
				switch event.Action {
				case "destroy":
					dc.logger.Debug("Docker container was destroyed:", zap.String("id", event.ID))
					dc.removeContainer(event.ID)
				default:
					dc.logger.Debug(
						"Docker container update:",
						zap.String("id", event.ID),
						zap.String("action", event.Action),
					)

					if container, ok := dc.inspectedContainerIsOfInterest(ctx, event.ID); ok {
						dc.persistContainer(container)
					}
				}

				if event.TimeNano > lastTime.UnixNano() {
					lastTime = time.Unix(0, event.TimeNano)
				}

			case err := <-errCh:
				// We are only interested when the context hasn't been canceled since requests made
				// with a closed context are guaranteed to fail.
				if ctx.Err() == nil {
					dc.logger.Error("Error watching docker container events", zap.Error(err))
					// Either decoding or connection error has occurred, so we should resume the event loop after
					// waiting a moment.  In cases of extended daemon unavailability this will retry until
					// collector teardown or background context is closed.
					select {
					case <-time.After(3 * time.Second):
						continue EVENT_LOOP
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}
}

// Queries inspect api and returns *ContainerJSON and true when container should be queried for stats,
// nil and false otherwise.
func (dc *dockerClient) inspectedContainerIsOfInterest(ctx context.Context, cid string) (*container, bool) {
	inspectCtx, cancel := context.WithTimeout(ctx, dc.config.Timeout)
	// container, err := dc.client.ContainerInspect(inspectCtx, cid)
	container, err := dc.backend.Inspect(inspectCtx, cid)
	defer cancel()
	if err != nil {
		dc.logger.Error(
			"Could not inspect updated container",
			zap.String("id", cid),
			zap.Error(err),
		)
	} else if !dc.shouldBeExcluded(container.Image) {
		return container, true
	}
	return nil, false
}

func (dc *dockerClient) persistContainer(container *container) {
	if container == nil {
		return
	}

	cid := container.ID
	if !container.StateRunning || container.StatePaused {
		dc.logger.Debug("Docker container not running.  Will not persist.", zap.String("id", cid))
		dc.removeContainer(cid)
		return
	}

	dc.logger.Debug("Monitoring Docker container", zap.String("id", cid))
	dc.containersLock.Lock()
	// container.EnvMap = containerEnvToMap(container.Env)
	dc.containers[cid] = container
	dc.containersLock.Unlock()
}

func (dc *dockerClient) removeContainer(cid string) {
	dc.containersLock.Lock()
	delete(dc.containers, cid)
	dc.containersLock.Unlock()
	dc.logger.Debug("Removed container from stores.", zap.String("id", cid))
}

func (dc *dockerClient) shouldBeExcluded(image string) bool {
	return dc.excludedImageMatcher != nil && dc.excludedImageMatcher.Matches(image)
}

func containerEnvToMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, v := range env {
		parts := strings.Split(v, "=")
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			continue
		}
		out[parts[0]] = parts[1]
	}
	return out
}
