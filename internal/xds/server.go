package xds

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"

	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	routeservice "github.com/envoyproxy/go-control-plane/envoy/service/route/v3"
	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"

	"google.golang.org/grpc"

	"github.com/envoyage/envoyage/internal/registry"
)

// Server is the xDS control plane server.
//
// Architecture:
//   Registry (service state)
//       │ onChange callback
//       ▼
//   SnapshotBuilder (registry → Envoy resources)
//       │
//       ▼
//   SnapshotCache (holds versioned snapshots per Envoy node)
//       │
//       ▼
//   go-control-plane Server (handles gRPC streams, ACK/NACK)
//       │
//       ▼
//   Envoy (subscribes via ADS)
//
// When the registry changes, we rebuild the entire snapshot and set it
// on the cache. go-control-plane handles diffing and streaming to Envoy.
type Server struct {
	cache   cachev3.SnapshotCache
	builder *SnapshotBuilder
	reg     *registry.Registry
	nodeID  string
	log     *slog.Logger
}

// NewServer creates an xDS server wired to the given registry.
//
// nodeID must match the node.id in Envoy's bootstrap config.
// Envoy identifies itself with this ID when subscribing to xDS.
// The cache uses it to look up which snapshot to serve.
func NewServer(reg *registry.Registry, nodeID string, log *slog.Logger) *Server {
	s := &Server{
		// hash.NewNodeHash() would allow different configs per Envoy node.
		// For now, we use IDHash which maps node.id directly to cache keys.
		cache:   cachev3.NewSnapshotCache(true, cachev3.IDHash{}, nil),
		builder: NewSnapshotBuilder(),
		reg:     reg,
		nodeID:  nodeID,
		log:     log,
	}

	// Wire up: registry change → rebuild snapshot → push to cache
	reg.OnChange(func() {
		if err := s.rebuildSnapshot(); err != nil {
			log.Error("failed to rebuild xDS snapshot", "error", err)
		}
	})

	return s
}

// rebuildSnapshot reads the current registry state, builds a new Envoy
// snapshot, validates it, and pushes it to the cache. go-control-plane
// then handles the gRPC streaming to connected Envoy instances.
func (s *Server) rebuildSnapshot() error {
	services, version := s.reg.Snapshot()

	snap, err := s.builder.Build(services, version)
	if err != nil {
		return fmt.Errorf("building snapshot v%d: %w", version, err)
	}

	if err := s.cache.SetSnapshot(context.Background(), s.nodeID, snap); err != nil {
		return fmt.Errorf("setting snapshot v%d: %w", version, err)
	}

	s.log.Info("pushed xDS snapshot", "version", version, "services", len(services))
	return nil
}

// Seed pushes an initial snapshot so Envoy has something to load on connect.
// Without this, Envoy would wait (potentially forever) for the first snapshot.
func (s *Server) Seed() error {
	return s.rebuildSnapshot()
}

// Serve starts the gRPC server on the given address (e.g. ":9090").
//
// This registers all xDS service handlers. Envoy connects here and subscribes
// to resource types (LDS, RDS, CDS, EDS, SDS) via a single ADS stream.
//
// ADS (Aggregated Discovery Service) multiplexes all resource types over one
// gRPC stream. This guarantees ordering: Envoy gets clusters before endpoints,
// listeners before routes, etc. Without ADS, race conditions can cause Envoy
// to reference a cluster that hasn't been delivered yet → NACK.
func (s *Server) Serve(ctx context.Context, addr string) error {
	// go-control-plane's server handles the xDS protocol:
	// stream management, ACK/NACK tracking, version diffing.
	xdsServer := serverv3.NewServer(ctx, s.cache, nil)

	grpcServer := grpc.NewServer()
	registerXDSServices(grpcServer, xdsServer)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}

	s.log.Info("xDS server listening", "addr", addr)

	// Graceful shutdown: when ctx is canceled, stop accepting new connections.
	go func() {
		<-ctx.Done()
		s.log.Info("shutting down xDS server")
		grpcServer.GracefulStop()
	}()

	return grpcServer.Serve(lis)
}

// registerXDSServices registers all xDS service handlers on the gRPC server.
// Each service corresponds to a resource type (LDS, RDS, CDS, EDS, SDS).
// The ADS handler is special — it aggregates all types on one stream.
func registerXDSServices(grpcServer *grpc.Server, xdsServer serverv3.Server) {
	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(grpcServer, xdsServer)
	clusterservice.RegisterClusterDiscoveryServiceServer(grpcServer, xdsServer)
	endpointservice.RegisterEndpointDiscoveryServiceServer(grpcServer, xdsServer)
	listenerservice.RegisterListenerDiscoveryServiceServer(grpcServer, xdsServer)
	routeservice.RegisterRouteDiscoveryServiceServer(grpcServer, xdsServer)
	secretservice.RegisterSecretDiscoveryServiceServer(grpcServer, xdsServer)
}
