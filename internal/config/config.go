// Package config loads ~/.config/tmux-sync/config.yaml and resolves named
// endpoints into ready-to-use Driver instances.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/christopherscot/tmux-sync/internal/driver"
	"gopkg.in/yaml.v3"
)

// Config is the on-disk schema for ~/.config/tmux-sync/config.yaml.
type Config struct {
	Endpoints map[string]Endpoint `yaml:"endpoints"`
}

// Endpoint is one named remote target. `kind` selects which fields apply.
type Endpoint struct {
	// Kind selects the driver: "k8s" | "ssh-docker" | "local".
	Kind string `yaml:"kind"`

	// k8s ----------------------------------------------------------------
	Context   string `yaml:"context,omitempty"`   // kubeconfig context (default: current)
	Namespace string `yaml:"namespace,omitempty"` // pod namespace
	Pod       string `yaml:"pod,omitempty"`       // explicit pod name
	Selector  string `yaml:"selector,omitempty"`  // OR a label selector (e.g., app=claude-session)
	Container string `yaml:"container,omitempty"` // optional, when the pod has >1 container

	// ssh-kubectl / ssh-docker ------------------------------------------
	Host    string   `yaml:"host,omitempty"`     // ssh host (alias or user@host)
	SSHArgs []string `yaml:"ssh_args,omitempty"` // extra ssh args, e.g. ["-o","ConnectTimeout=8"]

	// ssh-docker ---------------------------------------------------------
	ContainerName string `yaml:"container_name,omitempty"` // remote docker container name

	// local --------------------------------------------------------------
	Image string `yaml:"image,omitempty"` // image used when standing the local container up
}

// Path returns the canonical config file path:
// $XDG_CONFIG_HOME/tmux-sync/config.yaml (or ~/.config/tmux-sync/config.yaml).
func Path() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "tmux-sync", "config.yaml"), nil
}

// Load reads and parses the config file. A missing file is NOT an error: it
// returns a zero Config so callers can produce a precise "endpoint X not
// configured" message at lookup time.
func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{Endpoints: map[string]Endpoint{}}, nil
		}
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if c.Endpoints == nil {
		c.Endpoints = map[string]Endpoint{}
	}
	return &c, nil
}

// Resolve builds a Driver for the named endpoint.
func (c *Config) Resolve(name string) (driver.Driver, error) {
	ep, ok := c.Endpoints[name]
	if !ok {
		p, _ := Path()
		return nil, fmt.Errorf("endpoint %q not defined in %s", name, p)
	}
	switch ep.Kind {
	case "k8s":
		if ep.Pod == "" && ep.Selector != "" {
			return nil, fmt.Errorf("endpoint %q: selector-based pod discovery is planned but not yet implemented; set `pod:` explicitly", name)
		}
		return driver.NewK8s(ep.Context, ep.Namespace, ep.Pod, ep.Container)
	case "ssh-kubectl":
		// Like k8s, but runs kubectl on a remote SSH host. For laptops that
		// can ssh to a cluster node but don't have a direct kubeconfig to
		// the cluster API.
		if ep.Pod == "" && ep.Selector != "" {
			return nil, fmt.Errorf("endpoint %q: selector-based pod discovery is planned but not yet implemented; set `pod:` explicitly", name)
		}
		return driver.NewSSHKubectl(ep.Host, ep.Context, ep.Namespace, ep.Pod, ep.Container, ep.SSHArgs)
	case "ssh-docker", "local":
		return nil, fmt.Errorf("endpoint %q: kind %q is planned but not yet implemented (see SPEC.md)", name, ep.Kind)
	case "":
		return nil, fmt.Errorf("endpoint %q: `kind` is required (k8s | ssh-docker | local)", name)
	default:
		return nil, fmt.Errorf("endpoint %q: unknown kind %q (expected k8s, ssh-docker, or local)", name, ep.Kind)
	}
}
