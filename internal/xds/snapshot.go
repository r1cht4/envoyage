package xds

import (
	"fmt"
	"time"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/envoyage/envoyage/internal/config"
	"github.com/envoyage/envoyage/internal/registry"
)

// SnapshotBuilder translates the service registry into per-node xDS snapshots.
//
// Split-Horizon Routing
//
// The same logical service (e.g. "nextcloud → 192.168.1.50:8080") is
// represented differently depending on which Envoy is being configured:
//
//	Home Envoy  → cluster target is the actual container IP:port
//	VPS Envoy   → cluster target is cfg.HomeEnvoyIngress() (WireGuard IP)
//
// This means the VPS Envoy never needs to know about internal container IPs.
// It only ever talks to the home Envoy, which handles the final hop locally.
// Swapping the WireGuard IP is the only production change needed.
type SnapshotBuilder struct {
	cfg *config.Config
}

func NewSnapshotBuilder(cfg *config.Config) *SnapshotBuilder {
	return &SnapshotBuilder{cfg: cfg}
}

// Build creates a complete xDS snapshot for a specific Envoy node.
func (b *SnapshotBuilder) Build(nodeID string, services []*registry.Service, version uint64) (*cachev3.Snapshot, error) {
	var (
		clusters  []types.Resource
		routes    []*route.VirtualHost
		listeners []types.Resource
	)

	versionStr := fmt.Sprintf("v%d", version)
	isEdge := nodeID != b.cfg.HomeNodeID

	for _, svc := range services {
		clusterName := fmt.Sprintf("cluster_%s", svc.Name)

		// Split-Horizon: VPS Envoy routes to home Envoy (WireGuard ingress).
		// Home Envoy routes directly to the app container.
		upstream := svc.Upstream
		if isEdge {
			upstream = b.cfg.HomeEnvoyIngress()
		}

		clusters = append(clusters, makeCluster(clusterName, upstream))
		routes = append(routes, makeVirtualHost(svc.Name, svc.Domain, clusterName))
	}

	routeConfig := makeRouteConfig("local_routes", routes)

	httpListener, err := makeHTTPListener("listener_http", 10000, "local_routes")
	if err != nil {
		return nil, fmt.Errorf("building listener: %w", err)
	}
	listeners = append(listeners, httpListener)

	snap, err := cachev3.NewSnapshot(
		versionStr,
		map[resource.Type][]types.Resource{
			resource.ClusterType:  clusters,
			resource.RouteType:    {routeConfig},
			resource.ListenerType: listeners,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("creating snapshot: %w", err)
	}

	if err := snap.Consistent(); err != nil {
		return nil, fmt.Errorf("snapshot consistency check failed: %w", err)
	}

	return snap, nil
}

func makeCluster(name, upstream string) *cluster.Cluster {
	host, port := splitHostPort(upstream)

	return &cluster.Cluster{
		Name: name,
		ClusterDiscoveryType: &cluster.Cluster_Type{
			Type: cluster.Cluster_STRICT_DNS,
		},
		ConnectTimeout: durationpb.New(5 * time.Second),
		LoadAssignment: &endpoint.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*endpoint.LocalityLbEndpoints{{
				LbEndpoints: []*endpoint.LbEndpoint{{
					HostIdentifier: &endpoint.LbEndpoint_Endpoint{
						Endpoint: &endpoint.Endpoint{
							Address: makeAddress(host, port),
						},
					},
				}},
			}},
		},
	}
}

func makeVirtualHost(name, domain, clusterName string) *route.VirtualHost {
	return &route.VirtualHost{
		Name:    name,
		Domains: []string{domain},
		Routes: []*route.Route{{
			Match: &route.RouteMatch{
				PathSpecifier: &route.RouteMatch_Prefix{Prefix: "/"},
			},
			Action: &route.Route_Route{
				Route: &route.RouteAction{
					ClusterSpecifier: &route.RouteAction_Cluster{
						Cluster: clusterName,
					},
				},
			},
		}},
	}
}

func makeRouteConfig(name string, virtualHosts []*route.VirtualHost) *route.RouteConfiguration {
	return &route.RouteConfiguration{
		Name:         name,
		VirtualHosts: virtualHosts,
	}
}

func makeHTTPListener(name string, port uint32, routeConfigName string) (*listener.Listener, error) {
	routerAny, err := anypb.New(&routerv3.Router{})
	if err != nil {
		return nil, fmt.Errorf("marshaling router config: %w", err)
	}

	httpConnMgr := &hcm.HttpConnectionManager{
		StatPrefix: "ingress_http",
		RouteSpecifier: &hcm.HttpConnectionManager_Rds{
			Rds: &hcm.Rds{
				ConfigSource: &core.ConfigSource{
					ConfigSourceSpecifier: &core.ConfigSource_Ads{
						Ads: &core.AggregatedConfigSource{},
					},
					ResourceApiVersion: core.ApiVersion_V3,
				},
				RouteConfigName: routeConfigName,
			},
		},
		HttpFilters: []*hcm.HttpFilter{{
			Name: wellknown.Router,
			ConfigType: &hcm.HttpFilter_TypedConfig{
				TypedConfig: routerAny,
			},
		}},
	}

	hcmAny, err := anypb.New(httpConnMgr)
	if err != nil {
		return nil, fmt.Errorf("marshaling HCM: %w", err)
	}

	return &listener.Listener{
		Name: name,
		Address: &core.Address{
			Address: &core.Address_SocketAddress{
				SocketAddress: &core.SocketAddress{
					Protocol: core.SocketAddress_TCP,
					Address:  "0.0.0.0",
					PortSpecifier: &core.SocketAddress_PortValue{
						PortValue: port,
					},
				},
			},
		},
		FilterChains: []*listener.FilterChain{{
			Filters: []*listener.Filter{{
				Name: wellknown.HTTPConnectionManager,
				ConfigType: &listener.Filter_TypedConfig{
					TypedConfig: hcmAny,
				},
			}},
		}},
	}, nil
}

func makeAddress(host string, port uint32) *core.Address {
	return &core.Address{
		Address: &core.Address_SocketAddress{
			SocketAddress: &core.SocketAddress{
				Protocol: core.SocketAddress_TCP,
				Address:  host,
				PortSpecifier: &core.SocketAddress_PortValue{
					PortValue: port,
				},
			},
		},
	}
}

func splitHostPort(upstream string) (string, uint32) {
	for i := len(upstream) - 1; i >= 0; i-- {
		if upstream[i] == ':' {
			host := upstream[:i]
			var port uint32
			for _, c := range upstream[i+1:] {
				port = port*10 + uint32(c-'0')
			}
			return host, port
		}
	}
	return upstream, 0
}