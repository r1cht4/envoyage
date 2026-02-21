# Envoyage — Tracer Bullet (xDS Core)

This is the first vertical slice of Envoyage: a Go-based xDS control plane that dynamically configures Envoy. No Docker discovery, no WireGuard, no TLS — just the core mechanism that everything else builds on.

## What this proves

1. **Our Go code can control Envoy dynamically** via go-control-plane
2. **Config changes propagate in real-time** — no Envoy restart needed
3. **The registry → snapshot → xDS pipeline works end-to-end**

## Architecture

```
┌──────────────────────────────────┐
│         Control Plane            │
│                                  │
│  HTTP API (:8080)                │
│    │                             │
│    ▼                             │
│  Registry ──onChange──► xDS Server (:9090, gRPC)
│                              │   │
└──────────────────────────────┼───┘
                               │ ADS (gRPC stream)
                               ▼
┌──────────────────────────────────┐
│         Envoy (:10000)           │
│                                  │
│  Bootstrap config points to      │
│  controlplane:9090               │
│                                  │
│  Everything else is dynamic:     │
│  • Listeners (LDS)               │
│  • Routes (RDS)                  │
│  • Clusters (CDS)                │
│  • Endpoints (EDS)               │
└──────────────────────────────────┘
         │
         ▼ (routes to)
┌─────────────┐  ┌─────────────┐
│   web-a     │  │   web-b     │
│  (upstream) │  │  (upstream) │
└─────────────┘  └─────────────┘
```

## Quick Start

```bash
# Prerequisites: Docker, Docker Compose, Go 1.23+

# 1. Generate go.sum (first time only)
go mod tidy

# 2. Start everything
make up

# 3. Run the tracer bullet test sequence
make test-add     # Register web-a, verify routing
make test-switch  # Switch to web-b, verify routing changes
make test-remove  # Remove service, verify 404

# Watch logs
make logs

# Tear down
make down
```

## Test Sequence

### Test 1: Add a service
```bash
make test-add
```
Registers `web.example.com → web-a:5678`. A request to Envoy with `Host: web.example.com` should return "Hello from upstream A".

### Test 2: Switch upstream (dynamic update)
```bash
make test-switch
```
Removes the service and re-adds it pointing to `web-b:5678`. Same domain, different backend. Should return "Hello from upstream B" — **without restarting Envoy**.

### Test 3: Remove service
```bash
make test-remove
```
Removes the service entirely. Requests should get HTTP 404.

## Project Structure

```
envoyage/
├── cmd/controlplane/
│   └── main.go              # Entry point, wires registry + xDS + HTTP API
├── internal/
│   ├── registry/
│   │   └── registry.go      # Thread-safe service store with change notifications
│   └── xds/
│       ├── server.go         # gRPC xDS server (wraps go-control-plane)
│       └── snapshot.go       # Translates services → Envoy resources
├── envoy/
│   └── bootstrap.yaml        # Static Envoy config (points to our xDS server)
├── docker-compose.yaml        # Test stack
├── Dockerfile                 # Multi-stage build for control plane
├── Makefile                   # Convenience commands
└── go.mod
```

## Go Concepts Used

### Goroutines and Channels
The xDS server runs in the main goroutine (blocking), while the HTTP API runs in a separate goroutine started with `go func() {...}()`. Signal handling uses a channel to coordinate shutdown.

### Interfaces and Dependency Injection
`go-control-plane` defines interfaces like `cache.SnapshotCache` and `server.Server`. We don't implement the xDS protocol ourselves — we plug our snapshot cache into their server implementation.

### Mutexes (sync.RWMutex)
The registry uses a read-write mutex. Multiple goroutines can read simultaneously (the HTTP list endpoint, the snapshot builder), but writes (add/remove service) are exclusive. `RLock()` for reads, `Lock()` for writes.

### Protobuf and Any Types
Envoy configs are Protocol Buffer messages. Go works with them as typed structs (e.g., `*cluster.Cluster`), but Envoy's filter chains use `google.protobuf.Any` — a wrapper that can hold any protobuf type. That's why we call `anypb.New()` to wrap the HttpConnectionManager before embedding it in a Listener.

### Context for Cancellation
`context.WithCancel` creates a cancellable context. When we receive SIGINT, we call `cancel()`, which propagates through the context tree and triggers graceful shutdown of the gRPC server.

## What's Next

After this tracer bullet works, the next steps are:

1. **Docker Socket Watcher** — Replace the HTTP API with a Docker event listener that auto-discovers containers with `envoyage.*` labels
2. **Config Validator** — Add pre-push validation before setting snapshots
3. **WireGuard Tunnel** — Connect a remote Envoy over WireGuard to this control plane
4. **TLS / SDS** — Add certificate management and Secret Discovery Service
