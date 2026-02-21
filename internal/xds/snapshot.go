package xds

import (
	"fmt"
	"time"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/envoyage/envoyage/internal/registry"
)

// SnapshotBuilder translates our service registry into Envoy xDS snapshots.
//
// Envoy's configuration model has 5 core resource types. Think of them as layers:
//
//   Listener (LDS)  — "What ports/addresses does Envoy listen on?"
//       │
//       ▼
//   Route (RDS)     — "Based on the Host header / path, where should traffic go?"
//       │
//       ▼
//   Cluster (CDS)   — "What does the target look like? (timeouts, protocol, LB policy)"
//       │
//       ▼
//   Endpoint (EDS)  — "What are the actual IP:port addresses of the target?"
//       │
//       ▼
//   Secret (SDS)    — "TLS certificates for listeners and upstream connections"
//
// Our job: Take a list of Services and produce resources for each layer.
type SnapshotBuilder struct{}

func NewSnapshotBuilder() *SnapshotBuilder {
	return &SnapshotBuilder{}
}

// Build creates a complete xDS snapshot from the current service registry state.
//
// A snapshot is an atomic, versioned bundle of all resource types. When we push
// a new snapshot, go-control-plane diffs it against the previous one and only
// sends changes to Envoy (via incremental or SotW xDS).
//
// The version string must change whenever the content changes — Envoy uses it
// to detect updates. We use the registry's monotonic version counter.
func (b *SnapshotBuilder) Build(services []*registry.Service, version uint64) (*cachev3.Snapshot, error) {
	var (
		clusters  []types.Resource
		routes    []*route.VirtualHost
		listeners []types.Resource
	)

	versionStr := fmt.Sprintf("v%d", version)

	for _, svc := range services {
		clusterName := fmt.Sprintf("cluster_%s", svc.Name)

		// Cluster with STRICT_DNS: Envoy resolves the hostname via DNS and
		// routes to the resulting IPs. The address is set inline in the cluster's
		// LoadAssignment — no separate EDS needed.
		clusters = append(clusters, makeCluster(clusterName, svc.Upstream))

		// VirtualHost: maps domain → cluster
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

	// Consistency check: validates that all referenced clusters exist,
	// all routes point to valid clusters, etc.
	if err := snap.Consistent(); err != nil {
		return nil, fmt.Errorf("snapshot consistency check failed: %w", err)
	}

	return snap, nil
}

func makeCluster(name, upstream string) *cluster.Cluster {
	host, port := splitHostPort(upstream)

	return &cluster.Cluster{
		Name: name,

		// STRICT_DNS: Envoy resolves the hostname and routes to all returned IPs.
		// This works great with Docker Compose service names (e.g. "my-app"),
		// because Docker's embedded DNS resolves them to container IPs.
		ClusterDiscoveryType: &cluster.Cluster_Type{
			Type: cluster.Cluster_STRICT_DNS,
		},

		ConnectTimeout: durationpb.New(5 * time.Second),

		// The actual address to connect to.
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
		Name: name,

		// Which Host headers this virtual host matches.
		// ["cloud.example.com"] means requests to that domain hit this route.
		Domains: []string{domain},

		Routes: []*route.Route{{
			// Match everything (prefix "/").
			Match: &route.RouteMatch{
				PathSpecifier: &route.RouteMatch_Prefix{Prefix: "/"},
			},
			// Forward to our cluster.
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

// makeHTTPListener creates an Envoy listener with an HTTP connection manager.
//
// The chain: Listener → FilterChain → NetworkFilter (HCM) → HttpFilter (Router)
//
// HCM (HttpConnectionManager) is Envoy's HTTP/1.1 and HTTP/2 codec.
// It's a network filter that parses HTTP and delegates to HTTP filters.
// The Router HTTP filter does the actual routing based on the RDS config.
func makeHTTPListener(name string, port uint32, routeConfigName string) (*listener.Listener, error) {
	// The router filter needs an explicit typed_config. Without it,
	// Envoy can't find the registered implementation and NACKs the listener.
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

	// Marshal HCM config into an Any so it can be embedded in the filter chain.
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

// splitHostPort parses "host:port" into components.
// Returns port=0 if no port is specified.
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
