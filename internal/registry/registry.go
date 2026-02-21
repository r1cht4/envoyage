package registry

import (
	"fmt"
	"sync"
)

// Service represents a single routable application.
// This is our internal model — the xDS layer translates it to Envoy resources.
type Service struct {
	Name     string // unique identifier, e.g. "nextcloud"
	Domain   string // FQDN for routing, e.g. "cloud.example.com"
	Upstream string // host:port of the actual application
	Port     uint32 // listener port (typically 443 for external, 80 for internal)
}

// Registry is a thread-safe, in-memory store for services.
// Will be backed by SQLite later, but in-memory is fine for the tracer bullet.
type Registry struct {
	mu       sync.RWMutex
	services map[string]*Service
	version  uint64

	// onChange is called whenever the registry is modified.
	// The xDS layer hooks into this to push new snapshots.
	onChange func()
}

func New() *Registry {
	return &Registry{
		services: make(map[string]*Service),
	}
}

// OnChange registers a callback that fires after every mutation.
// Only one callback is supported — this is intentional for simplicity.
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

	// Fire callback AFTER releasing the lock. onChange triggers a snapshot
	// rebuild which needs a read lock — calling it under write lock deadlocks.
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
// or an agent re-registers with different upstream.
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

// Snapshot returns a copy of all services and the current version.
// The version is a monotonically increasing counter used for xDS snapshot versioning.
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
