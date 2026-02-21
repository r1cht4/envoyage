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

	"github.com/envoyage/envoyage/internal/config"
	"github.com/envoyage/envoyage/internal/registry"
)

// Server is the xDS control plane server.
type Server struct {
	cache   cachev3.SnapshotCache
	builder *SnapshotBuilder
	reg     *registry.Registry
	cfg     *config.Config
	log     *slog.Logger
}

func NewServer(reg *registry.Registry, cfg *config.Config, log *slog.Logger) *Server {
	s := &Server{
		cache:   cachev3.NewSnapshotCache(true, cachev3.IDHash{}, nil),
		builder: NewSnapshotBuilder(cfg),
		reg:     reg,
		cfg:     cfg,
		log:     log,
	}

	reg.OnChange(func() {
		if err := s.rebuildSnapshots(); err != nil {
			log.Error("failed to rebuild xDS snapshots", "error", err)
		}
	})

	return s
}

func (s *Server) rebuildSnapshots() error {
	services, version := s.reg.Snapshot()

	for _, nodeID := range s.cfg.NodeIDs() {
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
		"nodes", len(s.cfg.NodeIDs()),
		"home_envoy_ingress", s.cfg.HomeEnvoyIngress(),
	)
	return nil
}

func (s *Server) Seed() error {
	return s.rebuildSnapshots()
}

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

func registerXDSServices(grpcServer *grpc.Server, xdsServer serverv3.Server) {
	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(grpcServer, xdsServer)
	clusterservice.RegisterClusterDiscoveryServiceServer(grpcServer, xdsServer)
	endpointservice.RegisterEndpointDiscoveryServiceServer(grpcServer, xdsServer)
	listenerservice.RegisterListenerDiscoveryServiceServer(grpcServer, xdsServer)
	routeservice.RegisterRouteDiscoveryServiceServer(grpcServer, xdsServer)
	secretservice.RegisterSecretDiscoveryServiceServer(grpcServer, xdsServer)
}