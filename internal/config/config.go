// Package config loads and validates the control plane configuration from
// environment variables. All settings have sensible defaults so the binary
// works out of the box for local development without any .env file.
//
// In production, copy .env.example to .env, fill in the values, and
// docker-compose will pick them up automatically.
package config

import (
	"fmt"
	"os"
)

// Config holds all runtime configuration for the control plane.
// Values are loaded once at startup via Load() and then treated as immutable.
type Config struct {
	// XDSAddr is the gRPC listen address for the xDS server.
	// Envoy connects here to receive dynamic configuration.
	XDSAddr string

	// APIAddr is the HTTP listen address for the management API.
	APIAddr string

	// HomeNodeID is the xDS node ID of the home Envoy instance.
	// Must match node.id in envoy/bootstrap-home.yaml.
	HomeNodeID string

	// VPSNodeID is the xDS node ID of the VPS/edge Envoy instance.
	// Must match node.id in envoy/bootstrap-vps.yaml.
	VPSNodeID string

	// HomeWGIP is the WireGuard interface IP of the home node.
	// The VPS Envoy uses this as the upstream address for all clusters,
	// routing all traffic through the WireGuard tunnel to the home Envoy.
	//
	// In Docker Compose simulation mode (no real WireGuard), set this to
	// the Docker service name of the home Envoy (e.g. "envoy-home").
	HomeWGIP string

	// HomeEnvoyPort is the port the home Envoy listens on for incoming
	// proxied traffic. The VPS Envoy forwards to HomeWGIP:HomeEnvoyPort.
	HomeEnvoyPort string
}

// HomeEnvoyIngress returns the full upstream address the VPS Envoy uses
// to reach the home Envoy: "HomeWGIP:HomeEnvoyPort".
func (c *Config) HomeEnvoyIngress() string {
	return fmt.Sprintf("%s:%s", c.HomeWGIP, c.HomeEnvoyPort)
}

// NodeIDs returns the list of all managed Envoy node IDs.
func (c *Config) NodeIDs() []string {
	return []string{c.HomeNodeID, c.VPSNodeID}
}

// Load reads configuration from environment variables.
// Missing variables fall back to defaults suitable for local Docker Compose
// development. An error is returned only if a required variable is empty
// after applying defaults (currently none are strictly required).
func Load() (*Config, error) {
	cfg := &Config{
		XDSAddr:       getEnv("ENVOYAGE_XDS_ADDR", ":9090"),
		APIAddr:       getEnv("ENVOYAGE_API_ADDR", ":8080"),
		HomeNodeID:    getEnv("ENVOYAGE_HOME_NODE_ID", "envoyage-envoy-home"),
		VPSNodeID:     getEnv("ENVOYAGE_VPS_NODE_ID", "envoyage-envoy-vps"),
		HomeWGIP:      getEnv("ENVOYAGE_HOME_WG_IP", "envoy-home"),
		HomeEnvoyPort: getEnv("ENVOYAGE_HOME_ENVOY_PORT", "10000"),
	}
	return cfg, nil
}

// getEnv returns the value of the environment variable named by key,
// or fallback if the variable is unset or empty.
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}