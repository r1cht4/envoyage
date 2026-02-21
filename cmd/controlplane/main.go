package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/envoyage/envoyage/internal/registry"
	"github.com/envoyage/envoyage/internal/xds"
)

const (
	xdsAddr = ":9090" // gRPC — Envoy connects here
	apiAddr = ":8080" // HTTP — management API for testing
)

// nodeIDs lists every Envoy instance this control plane manages.
// Each gets a tailored snapshot: home Envoy routes to local containers,
// VPS Envoy routes everything to the home Envoy (via the WireGuard tunnel,
// simulated here as a plain Docker network connection).
var nodeIDs = []string{
	"envoyage-envoy-home", // Home node: routes to actual app containers
	"envoyage-envoy-vps",  // Edge node: routes to the home Envoy
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// --- Registry ---
	// Holds all known services. In the full product, this is backed by SQLite
	// and populated by Docker discovery. For the tracer bullet, we use the
	// management API to add/remove services manually.
	reg := registry.New()

	// --- xDS Server ---
	// Translates registry state into per-node Envoy configs and serves them via gRPC.
	xdsServer := xds.NewServer(reg, nodeIDs, log)

	// Seed an initial empty snapshot for every node. Without this, Envoy blocks
	// on connect waiting for resources that never arrive.
	if err := xdsServer.Seed(); err != nil {
		log.Error("failed to seed xDS", "error", err)
		os.Exit(1)
	}

	// --- Management API ---
	// Simple REST API for testing. Add/remove services, see what Envoy does.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /services", handleAddService(reg, log))
	mux.HandleFunc("DELETE /services/{name}", handleRemoveService(reg, log))
	mux.HandleFunc("GET /services", handleListServices(reg))

	// --- Startup ---
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle SIGINT/SIGTERM for clean shutdown.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Info("received shutdown signal")
		cancel()
	}()

	// Start the management API in a goroutine.
	go func() {
		log.Info("management API listening", "addr", apiAddr)
		if err := http.ListenAndServe(apiAddr, mux); err != nil {
			log.Error("management API failed", "error", err)
		}
	}()

	// Start the xDS gRPC server (blocks until ctx is canceled).
	if err := xdsServer.Serve(ctx, xdsAddr); err != nil {
		log.Error("xDS server failed", "error", err)
		os.Exit(1)
	}
}

// --- HTTP Handlers ---

// serviceRequest is the JSON body for adding a service.
//
//	POST /services
//	{"name": "web", "domain": "web.example.com", "upstream": "web-app:8080"}
type serviceRequest struct {
	Name     string `json:"name"`
	Domain   string `json:"domain"`
	Upstream string `json:"upstream"`
}

func handleAddService(reg *registry.Registry, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req serviceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if req.Name == "" || req.Domain == "" || req.Upstream == "" {
			http.Error(w, "name, domain, and upstream are required", http.StatusBadRequest)
			return
		}

		svc := &registry.Service{
			Name:     req.Name,
			Domain:   req.Domain,
			Upstream: req.Upstream,
		}

		if err := reg.Add(svc); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}

		log.Info("service added", "name", svc.Name, "domain", svc.Domain, "upstream", svc.Upstream)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "added %s → %s\n", svc.Domain, svc.Upstream)
	}
}

func handleRemoveService(reg *registry.Registry, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := reg.Remove(name); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		log.Info("service removed", "name", name)
		fmt.Fprintf(w, "removed %s\n", name)
	}
}

func handleListServices(reg *registry.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		services, version := reg.Snapshot()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"version":  version,
			"services": services,
		})
	}
}
