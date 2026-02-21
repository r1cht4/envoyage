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

	"github.com/envoyage/envoyage/internal/docker"
	"github.com/envoyage/envoyage/internal/registry"
	"github.com/envoyage/envoyage/internal/xds"
)

const (
	xdsAddr = ":9090" // gRPC — Envoy connects here
	apiAddr = ":8080" // HTTP — management API (debug / manual override)
)

// nodeIDs lists every Envoy instance this control plane manages.
// Each gets a tailored snapshot: home Envoy routes to local containers,
// VPS Envoy routes everything to the home Envoy (simulating the WireGuard
// tunnel in production).
var nodeIDs = []string{
	"envoyage-envoy-home",
	"envoyage-envoy-vps",
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// --- Registry ---
	// Central in-memory store for all known services.
	// Populated by two sources in parallel:
	//   1. Docker Watcher (automatic, label-based)
	//   2. Management API (manual, for testing and overrides)
	reg := registry.New()

	// --- xDS Server ---
	xdsServer := xds.NewServer(reg, nodeIDs, log)

	if err := xdsServer.Seed(); err != nil {
		log.Error("failed to seed xDS", "error", err)
		os.Exit(1)
	}

	// --- Docker Watcher ---
	// Watches the Docker socket for containers with envoyage.* labels.
	// Optional: if the socket is not mounted, we fall back to manual API only.
	watcher, err := docker.NewWatcher(reg, log)
	if err != nil {
		log.Warn("docker watcher unavailable, falling back to manual API only",
			"error", err)
	}

	// --- Management API ---
	// Stays active alongside the Docker watcher for debugging and overrides.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /services", handleAddService(reg, log))
	mux.HandleFunc("DELETE /services/{name}", handleRemoveService(reg, log))
	mux.HandleFunc("GET /services", handleListServices(reg))

	// --- Startup ---
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Info("received shutdown signal")
		cancel()
	}()

	if watcher != nil {
		go func() {
			if err := watcher.Run(ctx); err != nil {
				log.Error("docker watcher error", "error", err)
			}
		}()
	}

	go func() {
		log.Info("management API listening", "addr", apiAddr)
		if err := http.ListenAndServe(apiAddr, mux); err != nil {
			log.Error("management API failed", "error", err)
		}
	}()

	if err := xdsServer.Serve(ctx, xdsAddr); err != nil {
		log.Error("xDS server failed", "error", err)
		os.Exit(1)
	}
}

// --- HTTP Handlers ---

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
		log.Info("service added via API", "name", svc.Name, "domain", svc.Domain, "upstream", svc.Upstream)
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
		log.Info("service removed via API", "name", name)
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