// Package docker implements automatic service discovery via the Docker socket.
//
// The Watcher subscribes to the Docker event stream and translates container
// lifecycle events into registry mutations. When a container with the right
// labels starts, it is registered as a service. When it stops, it is removed.
//
// Label reference (add to any docker-compose.yml service):
//
//	envoyage.enable: "true"            # required — opt this container in
//	envoyage.domain: "app.example.com" # required — virtual host domain
//	envoyage.port:   "8080"            # required — port the app listens on
//	envoyage.name:   "myapp"           # optional — override service name
//
// If envoyage.name is not set, the name is derived from the Docker Compose
// service label (com.docker.compose.service) or the container name.
package docker

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	dockerclient "github.com/docker/docker/client"

	"github.com/envoyage/envoyage/internal/registry"
)

// Label keys the watcher looks for on containers.
const (
	labelEnable = "envoyage.enable"
	labelDomain = "envoyage.domain"
	labelPort   = "envoyage.port"
	labelName   = "envoyage.name"

	// Docker Compose sets this automatically on every container it manages.
	// We use it as a fallback service name when envoyage.name is not set.
	labelComposeSvc = "com.docker.compose.service"
)

// Watcher watches the Docker socket and keeps the registry in sync with
// running containers that have the appropriate labels.
type Watcher struct {
	client *dockerclient.Client
	reg    *registry.Registry
	log    *slog.Logger
}

// NewWatcher creates a Watcher connected to the local Docker daemon.
// Reads DOCKER_HOST / DOCKER_CERT_PATH / DOCKER_TLS_VERIFY from the environment,
// with automatic API version negotiation so it works across daemon versions.
func NewWatcher(reg *registry.Registry, log *slog.Logger) (*Watcher, error) {
	cli, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to Docker daemon: %w", err)
	}
	return &Watcher{client: cli, reg: reg, log: log}, nil
}

// Run starts the watcher. It first syncs already-running containers, then
// listens for new events until ctx is canceled.
//
// Call this in a goroutine alongside the xDS and HTTP servers.
func (w *Watcher) Run(ctx context.Context) error {
	w.log.Info("docker watcher starting")

	// Sync containers that were already running when we started.
	// Handles control plane restarts: existing containers are re-registered
	// without waiting for a container start event.
	if err := w.syncExisting(ctx); err != nil {
		w.log.Warn("initial container sync failed", "error", err)
	}

	// Subscribe to container events only.
	f := filters.NewArgs()
	f.Add("type", string(events.ContainerEventType))

	eventCh, errCh := w.client.Events(ctx, events.ListOptions{Filters: f})

	for {
		select {
		case <-ctx.Done():
			w.log.Info("docker watcher stopped")
			return nil
		case err := <-errCh:
			if ctx.Err() != nil {
				return nil // normal shutdown
			}
			return fmt.Errorf("docker event stream: %w", err)
		case event := <-eventCh:
			w.handleEvent(ctx, event)
		}
	}
}

// syncExisting registers all currently running containers with envoyage labels.
func (w *Watcher) syncExisting(ctx context.Context) error {
	containers, err := w.client.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}

	registered := 0
	for _, c := range containers {
		if c.Labels[labelEnable] != "true" {
			continue
		}
		if err := w.registerByID(ctx, c.ID); err != nil {
			w.log.Warn("skipping container during sync",
				"id", shortID(c.ID),
				"error", err,
			)
			continue
		}
		registered++
	}

	w.log.Info("initial sync complete",
		"scanned", len(containers),
		"registered", registered,
	)
	return nil
}

// handleEvent processes a single Docker container event.
func (w *Watcher) handleEvent(ctx context.Context, event events.Message) {
	switch event.Action {
	case events.ActionStart:
		if err := w.registerByID(ctx, event.Actor.ID); err != nil {
			w.log.Warn("failed to register container on start",
				"id", shortID(event.Actor.ID),
				"error", err,
			)
		}

	case events.ActionStop, events.ActionDie, events.ActionKill:
		// The container may already be gone by the time we handle this event,
		// so we use the event actor attributes (set at event time, always
		// available) rather than inspecting the possibly-gone container.
		attrs := event.Actor.Attributes
		if attrs[labelEnable] != "true" {
			return
		}
		name := serviceName(attrs)
		if name == "" {
			return
		}
		if err := w.reg.Remove(name); err != nil {
			// Expected if the container was never registered (e.g. missing labels).
			w.log.Debug("container not in registry on stop", "name", name)
		} else {
			w.log.Info("docker: service removed", "name", name, "action", string(event.Action))
		}
	}
}

// registerByID inspects a container by ID, validates its labels, resolves its
// IP address, and upserts it into the registry.
func (w *Watcher) registerByID(ctx context.Context, id string) error {
	info, err := w.client.ContainerInspect(ctx, id)
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", shortID(id), err)
	}

	labels := info.Config.Labels

	if labels[labelEnable] != "true" {
		return nil // not opted in
	}

	// Validate required labels.
	domain := labels[labelDomain]
	if domain == "" {
		return fmt.Errorf("missing required label %q", labelDomain)
	}
	portStr := labels[labelPort]
	if portStr == "" {
		return fmt.Errorf("missing required label %q", labelPort)
	}
	port, err := strconv.ParseUint(portStr, 10, 32)
	if err != nil {
		return fmt.Errorf("invalid label %q=%q: %w", labelPort, portStr, err)
	}

	// We use the actual IP rather than the Docker DNS name because:
	//   a) The home Envoy may not be in the same Docker network.
	//   b) IPs are unambiguous across compose projects with identical service names.
	//   c) In a future phase, the registry stores both the local IP (home Envoy)
	//      and the WireGuard hop (VPS Envoy) — the IP is the canonical local addr.
	ip, err := containerIP(info)
	if err != nil {
		return fmt.Errorf("resolving IP for %s: %w", shortID(id), err)
	}

	name := serviceName(labels)
	if name == "" {
		name = strings.TrimPrefix(info.Name, "/")
	}

	svc := &registry.Service{
		Name:     name,
		Domain:   domain,
		Upstream: fmt.Sprintf("%s:%d", ip, port),
	}

	// Upsert: try Add, fall back to Update on conflict.
	// Makes registration idempotent across syncExisting + event-driven paths.
	if err := w.reg.Add(svc); err != nil {
		if err2 := w.reg.Update(svc); err2 != nil {
			return fmt.Errorf("upserting %q: %w", name, err2)
		}
		w.log.Info("docker: service updated",
			"name", name, "domain", domain, "upstream", svc.Upstream)
	} else {
		w.log.Info("docker: service registered",
			"name", name, "domain", domain, "upstream", svc.Upstream)
	}
	return nil
}

// containerIP returns the IP address of a container, choosing the best network.
//
// Selection order:
//  1. Any network whose name contains "envoyage" (the dedicated proxy mesh).
//  2. The first network with a non-empty IP address (compose project network).
func containerIP(info types.ContainerJSON) (string, error) {
	networks := info.NetworkSettings.Networks
	if len(networks) == 0 {
		return "", fmt.Errorf("container has no attached networks")
	}

	// Prefer our named mesh network.
	for name, net := range networks {
		if strings.Contains(strings.ToLower(name), "envoyage") && net.IPAddress != "" {
			return net.IPAddress, nil
		}
	}

	// Fall back to first available IP.
	for _, net := range networks {
		if net.IPAddress != "" {
			return net.IPAddress, nil
		}
	}

	return "", fmt.Errorf("no IP address found in any attached network")
}

// serviceName derives a stable unique name from a label map.
//
//  1. envoyage.name (explicit user override — highest priority)
//  2. com.docker.compose.service (auto-set by Compose on every container)
//  3. Empty string — caller falls back to container name
func serviceName(labels map[string]string) string {
	if v := labels[labelName]; v != "" {
		return v
	}
	if v := labels[labelComposeSvc]; v != "" {
		return v
	}
	return ""
}

// shortID returns the first 12 characters of a Docker container ID,
// matching the format used by docker ps and docker logs.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}