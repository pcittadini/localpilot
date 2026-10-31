package models

import (
	"fmt"

	"localpilot/internal/config"
)

// Spec binds a configured model to the provider that serves it.
type Spec struct {
	config.ModelConfig
	Provider Provider
}

// Registry resolves logical model names from configuration.
type Registry struct {
	specs map[string]Spec
	order []string
	def   string
}

// NewRegistry builds a registry from configuration. providers maps provider
// names (as used in config) to implementations.
func NewRegistry(cfg *config.Config, providers map[string]Provider) (*Registry, error) {
	r := &Registry{specs: map[string]Spec{}, def: cfg.Models.Default}
	for _, m := range cfg.Models.Available {
		p, ok := providers[m.Provider]
		if !ok {
			return nil, fmt.Errorf("model %q: no provider %q", m.Name, m.Provider)
		}
		r.specs[m.Name] = Spec{ModelConfig: m, Provider: p}
		r.order = append(r.order, m.Name)
	}
	if _, ok := r.specs[r.def]; !ok {
		return nil, fmt.Errorf("default model %q is not configured", r.def)
	}
	return r, nil
}

// Get returns the named model, or the default when name is empty.
func (r *Registry) Get(name string) (Spec, error) {
	if name == "" {
		name = r.def
	}
	s, ok := r.specs[name]
	if !ok {
		return Spec{}, fmt.Errorf("unknown model %q", name)
	}
	return s, nil
}

// All returns every configured model in configuration order.
func (r *Registry) All() []Spec {
	out := make([]Spec, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.specs[n])
	}
	return out
}
