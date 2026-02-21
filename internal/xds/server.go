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
//
//	Registry (service state)
//	    │ onChange callback
//	    ▼
//	SnapshotBuilder (registry → per-node Envoy resources)
//	    │ one snapshot per nodeID
//	    ▼
//	SnapshotCache (versioned snapshots keyed by node ID)
//	    │
//	    ▼
//	go-control-plane Server (gRPC streams, ACK/NACK)
//	    │
//	    ├── envoyage-envoy-home → clusters point to local containers
//	    └── envoyage-envoy-vps  → clusters point to envoy-home:10000
//
// Split-Horizon routing: both Envoys subscribe to the same control plane,
// but each receives a different view of the world tailored to its role.
// The home Envoy knows the real upstreams; the VPS Envoy only ever talks
// to the home Envoy (simulating the WireGuard tunnel in production).
type Server struct {
	cache   cachev3.SnapshotCache
	builder *SnapshotBuilder
	reg     *registry.Registry
	nodeIDs []string
	log     *slog.Logger
}

// NewServer creates an xDS server wired to the given registry.
//
// nodeIDs lists every Envoy instance the control plane manages.
// Each node must set a matching node.id in its Envoy bootstrap config.
func NewServer(reg *registry.Registry, nodeIDs []string, log *slog.Logger) *Server {
	s := &Server{
		// IDHash maps node.id strings directly to cache keys.
		// NodeHash would allow more complex grouping — not needed yet.
		cache:   cachev3.NewSnapshotCache(true, cachev3.IDHash{}, nil),
		builder: NewSnapshotBuilder(),
		reg:     reg,
		nodeIDs: nodeIDs,
		log:     log,
	}

	// Wire up: every registry mutation → rebuild all per-node snapshots.
	reg.OnChange(func() {
		if err := s.rebuildSnapshots(); err != nil {
			log.Error("failed to rebuild xDS snapshots", "error", err)
		}
	})

	return s
}

// rebuildSnapshots reads the current registry state and pushes a fresh,
// tailored snapshot into the cache for every registered node.
//
// go-control-plane handles the downstream gRPC streaming to connected Envoys.
func (s *Server) rebuildSnapshots() error {
	services, version := s.reg.Snapshot()

	for _, nodeID := range s.nodeIDs {
		snap, err := s.builder.Build(nodeID, services, version)
		if err != nil {
			return fmt.Errorf("building snapshot v%d for node %q: %w", version, nodeID, err)
		}

		if err := s.cache.SetSnapshot(context.Background(), nodeID, snap); err != nil {
			return fmt.Errorf("setting snapshot v%d for node %q: %w", version, nodeID, err)
		}
	}

	s.log.Info("pushed xDS snapshots",
		   "version", version,
	    "services", len(services),
		   "nodes", len(s.nodeIDs),
	)
	return nil
}

// Seed pushes an initial empty snapshot for every node so that Envoy has
// something to load immediately on connect and does not stall.
func (s *Server) Seed() error {
	return s.rebuildSnapshots()
}

// Serve starts the gRPC server on the given address (e.g. ":9090").
//
// All xDS service types (LDS, RDS, CDS, EDS, SDS) are registered and
// multiplexed over a single ADS stream. ADS guarantees ordering:
// clusters arrive before routes, listeners after their dependencies.
// Without ADS, race conditions can cause Envoy to NACK a listener that
// references a cluster that hasn't been delivered yet.
func (s *Server) Serve(ctx context.Context, addr string) error {
	xdsServer := serverv3.NewServer(ctx, s.cache, nil)

	grpcServer := grpc.NewServer()
	registerXDSServices(grpcServer, xdsServer)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}

	s.log.Info("xDS server listening", "addr", addr)

	go func() {
		<-ctx.Done()
		s.log.Info("shutting down xDS server")
		grpcServer.GracefulStop()
	}()

	return grpcServer.Serve(lis)
}

// registerXDSServices registers all resource-type handlers on the gRPC server.
// The ADS handler is the critical one — it aggregates all types on one stream.
func registerXDSServices(grpcServer *grpc.Server, xdsServer serverv3.Server) {
	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(grpcServer, xdsServer)
	clusterservice.RegisterClusterDiscoveryServiceServer(grpcServer, xdsServer)
	endpointservice.RegisterEndpointDiscoveryServiceServer(grpcServer, xdsServer)
	listenerservice.RegisterListenerDiscoveryServiceServer(grpcServer, xdsServer)
	routeservice.RegisterRouteDiscoveryServiceServer(grpcServer, xdsServer)
	secretservice.RegisterSecretDiscoveryServiceServer(grpcServer, xdsServer)
}
