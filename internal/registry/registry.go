package registry

import (
	"fmt"
	"sync"
)

// Service represents a single routable application.
// This is our internal model — the xDS layer translates it to Envoy resources.
//
// Upstream is stored as "host:port" from the registrant's perspective (i.e.
// as the home node sees it). The SnapshotBuilder rewrites the target for edge
// nodes transparently — callers never need to know about Split-Horizon routing.
type Service struct {
	Name     string // unique identifier, e.g. "nextcloud"
	Domain   string // FQDN for virtual-host matching, e.g. "cloud.example.com"
	Upstream string // host:port of the actual app, e.g. "web-a:5678"
}

// Registry is a thread-safe, in-memory store for services.
// Will be backed by SQLite and populated by Docker discovery in a later phase.
type Registry struct {
	mu       sync.RWMutex
	services map[string]*Service
	version  uint64

	// onChange is called after every mutation, outside the write lock.
	// The xDS server hooks into this to push fresh snapshots to all Envoys.
	// Only one callback is supported — intentional, keeps the coupling simple.
	onChange func()
}

func New() *Registry {
	return &Registry{
		services: make(map[string]*Service),
	}
}

// OnChange registers the function to be called after each registry mutation.
func (r *Registry) OnChange(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onChange = fn
}

func (r *Registry) Add(svc *Service) error {
	r.mu.Lock()

	if _, exists := r.services[svc.Name]; exists {
		r.mu.Unlock()
		return fmt.Errorf("service %q already exists", svc.Name)
	}

	r.services[svc.Name] = svc
	r.version++
	cb := r.onChange
	r.mu.Unlock()

	// Fire callback AFTER releasing the lock.
	// onChange triggers a snapshot rebuild which needs a read lock —
	// calling it under the write lock would deadlock.
	if cb != nil {
		cb()
	}
	return nil
}

func (r *Registry) Remove(name string) error {
	r.mu.Lock()

	if _, exists := r.services[name]; !exists {
		r.mu.Unlock()
		return fmt.Errorf("service %q not found", name)
	}

	delete(r.services, name)
	r.version++
	cb := r.onChange
	r.mu.Unlock()

	if cb != nil {
		cb()
	}
	return nil
}

// Update replaces an existing service. Useful when Docker labels change
// or an agent re-registers with a different upstream.
func (r *Registry) Update(svc *Service) error {
	r.mu.Lock()

	if _, exists := r.services[svc.Name]; !exists {
		r.mu.Unlock()
		return fmt.Errorf("service %q not found", svc.Name)
	}

	r.services[svc.Name] = svc
	r.version++
	cb := r.onChange
	r.mu.Unlock()

	if cb != nil {
		cb()
	}
	return nil
}

// Snapshot returns a copy of all services and the current version counter.
// The version is monotonically increasing and used for xDS snapshot versioning.
func (r *Registry) Snapshot() ([]*Service, uint64) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*Service, 0, len(r.services))
	for _, svc := range r.services {
		cp := *svc
		out = append(out, &cp)
	}
	return out, r.version
}
