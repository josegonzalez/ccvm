package backend

import (
	"fmt"
	"sort"

	"github.com/josegonzalez/ccvm/internal/run"
)

// Config carries the settings a backend needs beyond the profile.
//
// Most backends need nothing here; proxmox and k8s need to know where their
// control plane is.
type Config struct {
	// ProxmoxURL is the base API URL, e.g. https://pve1:8006.
	ProxmoxURL string
	// ProxmoxNode is the node to place guests on. Empty means pick the least
	// loaded.
	ProxmoxNode string
	// ProxmoxTokenID and ProxmoxSecret authenticate as an API token.
	ProxmoxTokenID string
	ProxmoxSecret  string
	// ProxmoxStorage is where clones land. Empty uses the template's storage.
	ProxmoxStorage string
	// ProxmoxInsecure skips TLS verification, for a homelab CA.
	ProxmoxInsecure bool

	// KubeNamespace is where session Jobs are created.
	KubeNamespace string
	// KubeContext selects a kubeconfig context. Empty uses the current one.
	KubeContext string
}

// Factory builds a backend.
type Factory func(run.Execer, Config) (Backend, error)

var registry = map[string]Factory{}

// Register adds a backend under name. It panics on a duplicate, which can only
// happen through a programming mistake at init time.
func Register(name string, f Factory) {
	if _, dup := registry[name]; dup {
		panic("backend: duplicate registration for " + name)
	}
	registry[name] = f
}

// New builds the named backend.
func New(name string, e run.Execer, cfg Config) (Backend, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q; available: %v", name, Names())
	}
	return f(e, cfg)
}

// Names lists the registered backends, sorted.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func init() {
	Register("docker", func(e run.Execer, _ Config) (Backend, error) {
		return NewDocker(e), nil
	})
}
