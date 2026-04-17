package proxy

import (
	"context"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// simpleClusterConfig represents cluster configuration for the proxy.
type simpleClusterConfig struct {
	Name      string `yaml:"name"`
	APIServer string `yaml:"apiServer"`
	Default   bool   `yaml:"default"`
}

// simpleClusterRegistryConfig is the top-level configuration.
type simpleClusterRegistryConfig struct {
	Clusters []simpleClusterConfig `yaml:"clusters"`
}

// fileBasedClusterResolver loads cluster configurations from a YAML file.
type fileBasedClusterResolver struct {
	clusters       map[string]*ClusterConfig
	defaultCluster string
}

// NewFileBasedClusterResolver creates a resolver that reads from a config file.
func NewFileBasedClusterResolver(configPath string) (ClusterResolver, error) {
	// If no config path, return single-cluster resolver
	if configPath == "" {
		return newSingleClusterResolver()
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("reading cluster config: %w", err)
	}

	var cfg simpleClusterRegistryConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing cluster config: %w", err)
	}

	if len(cfg.Clusters) == 0 {
		return nil, fmt.Errorf("no clusters defined in config")
	}

	resolver := &fileBasedClusterResolver{
		clusters: make(map[string]*ClusterConfig),
	}

	for _, c := range cfg.Clusters {
		resolver.clusters[c.Name] = &ClusterConfig{
			Name:      c.Name,
			APIServer: c.APIServer,
		}
		if c.Default {
			resolver.defaultCluster = c.Name
		}
	}

	// If no default specified, use first cluster
	if resolver.defaultCluster == "" && len(cfg.Clusters) > 0 {
		resolver.defaultCluster = cfg.Clusters[0].Name
	}

	return resolver, nil
}

// newSingleClusterResolver creates a resolver for single-cluster mode (backward compat).
// It reads the API server from the environment or uses a placeholder.
func newSingleClusterResolver() (ClusterResolver, error) {
	apiServer := os.Getenv("KUBERNETES_SERVICE_HOST")
	if apiServer != "" {
		apiServer = fmt.Sprintf("https://%s:%s", apiServer, os.Getenv("KUBERNETES_SERVICE_PORT"))
	} else {
		apiServer = "https://kubernetes.default.svc"
	}

	return &fileBasedClusterResolver{
		clusters: map[string]*ClusterConfig{
			"local": {
				Name:      "local",
				APIServer: apiServer,
			},
		},
		defaultCluster: "local",
	}, nil
}

func (r *fileBasedClusterResolver) GetCluster(ctx context.Context, name string) (*ClusterConfig, error) {
	if cfg, ok := r.clusters[name]; ok {
		return cfg, nil
	}
	return nil, fmt.Errorf("cluster %q not found", name)
}

func (r *fileBasedClusterResolver) GetDefaultCluster(ctx context.Context) (*ClusterConfig, error) {
	if r.defaultCluster == "" {
		return nil, fmt.Errorf("no default cluster configured")
	}
	return r.GetCluster(ctx, r.defaultCluster)
}
