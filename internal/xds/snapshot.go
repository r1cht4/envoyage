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

	"github.com/envoyage/envoyage/internal/registry"
)

// homeEnvoyNodeID is the canonical identifier for the home Envoy instance.
// Must match node.id in envoy/bootstrap-home.yaml.
const homeEnvoyNodeID = "envoyage-envoy-home"

// homeEnvoyIngress is the address the VPS Envoy uses to reach the home Envoy.
// In Docker Compose this is the service name + listener port.
// In production this will be the WireGuard IP of the home node.
const homeEnvoyIngress = "envoy-home:10000"

// SnapshotBuilder translates the service registry into per-node xDS snapshots.
//
// Split-Horizon Routing
//
// The same logical service (e.g. "nextcloud → 192.168.1.50:8080") must be
// represented differently depending on which Envoy is asking:
//
//	Home Envoy (envoyage-envoy-home)
//	  Cluster target: the actual app container (svc.Upstream)
//	  Lives on the same host as the containers, so Docker DNS works.
//
//	VPS / Edge Envoy (envoyage-envoy-vps)
//	  Cluster target: envoy-home:10000  (the home Envoy's ingress port)
//	  Can't reach internal container IPs; WireGuard tunnel leads to home Envoy.
//	  The home Envoy then re-routes based on the Host header.
//
// Both nodes share the same virtual host / domain configuration — only the
// cluster endpoint differs. This means:
//   - Domain-based routing works identically on both sides.
//   - In production, swapping homeEnvoyIngress to a WireGuard IP is the only
//     change needed to make the VPS Envoy work over the real tunnel.
//
// Envoy xDS resource hierarchy (reminder):
//
//	Listener (LDS)  — which port to listen on
//	  └─ Route (RDS)    — match Host header → cluster name
//	       └─ Cluster (CDS)  — upstream settings (timeout, LB policy)
//	            └─ Endpoint (EDS) — actual IP:port to connect to
//	                  └─ Secret (SDS) — TLS certificates
type SnapshotBuilder struct{}

func NewSnapshotBuilder() *SnapshotBuilder {
	return &SnapshotBuilder{}
}

// Build creates a complete xDS snapshot for a specific Envoy node.
//
// The nodeID parameter drives the Split-Horizon decision: home nodes get
// direct container upstreams, edge nodes get the home Envoy as their upstream.
//
// A snapshot is an atomic, versioned bundle of all resource types. Pushing a
// new snapshot makes go-control-plane diff it against the previous one and
// stream only the changed resources to the connected Envoy.
func (b *SnapshotBuilder) Build(nodeID string, services []*registry.Service, version uint64) (*cachev3.Snapshot, error) {
	var (
		clusters  []types.Resource
		routes    []*route.VirtualHost
		listeners []types.Resource
	)

	versionStr := fmt.Sprintf("v%d", version)
	isEdge := nodeID != homeEnvoyNodeID

	for _, svc := range services {
		clusterName := fmt.Sprintf("cluster_%s", svc.Name)

		// Split-Horizon: choose upstream based on which node we're building for.
		//
		// Edge (VPS):
		//   All traffic → home Envoy's ingress port. The home Envoy carries out
		//   the actual per-service routing based on the Host header it receives.
		//   In production, homeEnvoyIngress will be the WireGuard peer IP.
		//
		// Home:
		//   Traffic → real app container. svc.Upstream is "host:port" as
		//   registered via Docker discovery or the management API.
		upstream := svc.Upstream
		if isEdge {
			upstream = homeEnvoyIngress
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

	// Consistency check: validates that all referenced clusters and routes
	// exist and are internally coherent before we push to Envoy.
	if err := snap.Consistent(); err != nil {
		return nil, fmt.Errorf("snapshot consistency check failed: %w", err)
	}

	return snap, nil
}

// makeCluster builds an Envoy Cluster resource for the given upstream address.
//
// STRICT_DNS: Envoy resolves the hostname on first use and periodically
// thereafter. Works well with Docker Compose service names (Docker's embedded
// DNS handles them) and with WireGuard peer hostnames in production.
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

// makeVirtualHost creates a VirtualHost that matches requests by Host header
// and forwards them to the named cluster.
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

// makeHTTPListener creates an Envoy Listener with an HTTP connection manager.
//
// Filter chain: Listener → FilterChain → HCM (network filter) → Router (HTTP filter)
//
// HCM parses HTTP/1.1 and HTTP/2 and delegates routing decisions to the Router
// filter, which consults the RDS route config delivered via ADS.
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

// splitHostPort parses "host:port" into components.
// Returns port 0 if no port separator is found.
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
