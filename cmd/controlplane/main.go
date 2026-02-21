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

	"github.com/envoyage/envoyage/internal/config"
	"github.com/envoyage/envoyage/internal/docker"
	"github.com/envoyage/envoyage/internal/registry"
	"github.com/envoyage/envoyage/internal/xds"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// --- Config ---
	cfg, err := config.Load()
	if err != nil {
		log.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	log.Info("config loaded",
		"xds_addr", cfg.XDSAddr,
		"api_addr", cfg.APIAddr,
		"home_node", cfg.HomeNodeID,
		"vps_node", cfg.VPSNodeID,
		"home_envoy_ingress", cfg.HomeEnvoyIngress(),
	)

	// --- Registry ---
	reg := registry.New()

	// --- xDS Server ---
	xdsServer := xds.NewServer(reg, cfg, log)
	if err := xdsServer.Seed(); err != nil {
		log.Error("failed to seed xDS", "error", err)
		os.Exit(1)
	}

	// --- Docker Watcher ---
	watcher, err := docker.NewWatcher(reg, log)
	if err != nil {
		log.Warn("docker watcher unavailable, falling back to manual API only", "error", err)
	}

	// --- Management API ---
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
		log.Info("management API listening", "addr", cfg.APIAddr)
		if err := http.ListenAndServe(cfg.APIAddr, mux); err != nil {
			log.Error("management API failed", "error", err)
		}
	}()

	if err := xdsServer.Serve(ctx, cfg.XDSAddr); err != nil {
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
		svc := &registry.Service{Name: req.Name, Domain: req.Domain, Upstream: req.Upstream}
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
		json.NewEncoder(w).Encode(map[string]any{"version": version, "services": services})
	}
}