package operator

import (
	"context"
	"fmt"
	"os"
	"sync"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ClusterConfig represents the configuration for a single cluster.
type ClusterConfig struct {
	Name                string `yaml:"name"`
	APIServer           string `yaml:"apiServer"`
	Default             bool   `yaml:"default"`
	Kubeconfig          string `yaml:"kubeconfig,omitempty"`
	ServiceAccountToken string `yaml:"serviceAccountToken,omitempty"`
	CACert              string `yaml:"caCert,omitempty"`
}

// ClusterRegistryConfig is the top-level configuration for all clusters.
type ClusterRegistryConfig struct {
	Clusters []ClusterConfig `yaml:"clusters"`
}

// ClusterClient wraps a Kubernetes client for a specific cluster.
type ClusterClient struct {
	Name      string
	APIServer string
	Client    client.Client
	Clientset *kubernetes.Clientset
}

// ClusterRegistry manages multiple cluster connections.
type ClusterRegistry interface {
	// GetCluster returns a configured Kubernetes client for the named cluster.
	GetCluster(ctx context.Context, name string) (*ClusterClient, error)

	// GetDefaultCluster returns the default cluster (for backward compatibility).
	GetDefaultCluster(ctx context.Context) (*ClusterClient, error)

	// ListClusters returns all registered cluster names.
	ListClusters(ctx context.Context) ([]string, error)
}

type clusterRegistry struct {
	mu       sync.RWMutex
	clusters map[string]*ClusterClient
	scheme   *runtime.Scheme
	config   *ClusterRegistryConfig
}

// NewClusterRegistry creates a new cluster registry from a configuration file.
func NewClusterRegistry(configPath string, scheme *runtime.Scheme) (ClusterRegistry, error) {
	// If no config file provided, return a single-cluster registry (backward compat)
	if configPath == "" {
		return newSingleClusterRegistry(scheme)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("reading cluster config: %w", err)
	}

	var cfg ClusterRegistryConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing cluster config: %w", err)
	}

	if len(cfg.Clusters) == 0 {
		return nil, fmt.Errorf("no clusters defined in config")
	}

	return &clusterRegistry{
		clusters: make(map[string]*ClusterClient),
		scheme:   scheme,
		config:   &cfg,
	}, nil
}

// newSingleClusterRegistry creates a registry with only the in-cluster config (backward compat).
func newSingleClusterRegistry(scheme *runtime.Scheme) (ClusterRegistry, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("getting in-cluster config: %w", err)
	}

	k8sClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes clientset: %w", err)
	}

	clusterClient := &ClusterClient{
		Name:      "local",
		APIServer: restConfig.Host,
		Client:    k8sClient,
		Clientset: clientset,
	}

	return &clusterRegistry{
		clusters: map[string]*ClusterClient{"local": clusterClient},
		scheme:   scheme,
		config: &ClusterRegistryConfig{
			Clusters: []ClusterConfig{{Name: "local", Default: true}},
		},
	}, nil
}

func (r *clusterRegistry) GetCluster(ctx context.Context, name string) (*ClusterClient, error) {
	r.mu.RLock()
	if c, ok := r.clusters[name]; ok {
		r.mu.RUnlock()
		return c, nil
	}
	r.mu.RUnlock()

	// Cluster not cached, load it
	return r.loadCluster(ctx, name)
}

func (r *clusterRegistry) GetDefaultCluster(ctx context.Context) (*ClusterClient, error) {
	for _, cfg := range r.config.Clusters {
		if cfg.Default {
			return r.GetCluster(ctx, cfg.Name)
		}
	}
	// If no default specified, use the first cluster
	if len(r.config.Clusters) > 0 {
		return r.GetCluster(ctx, r.config.Clusters[0].Name)
	}
	return nil, fmt.Errorf("no clusters configured")
}

func (r *clusterRegistry) ListClusters(ctx context.Context) ([]string, error) {
	names := make([]string, 0, len(r.config.Clusters))
	for _, cfg := range r.config.Clusters {
		names = append(names, cfg.Name)
	}
	return names, nil
}

func (r *clusterRegistry) loadCluster(ctx context.Context, name string) (*ClusterClient, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check after acquiring lock
	if c, ok := r.clusters[name]; ok {
		return c, nil
	}

	// Find the cluster config
	var clusterCfg *ClusterConfig
	for i := range r.config.Clusters {
		if r.config.Clusters[i].Name == name {
			clusterCfg = &r.config.Clusters[i]
			break
		}
	}
	if clusterCfg == nil {
		return nil, fmt.Errorf("cluster %q not found in configuration", name)
	}

	// Build the rest config based on the cluster config
	restConfig, err := r.buildRestConfig(clusterCfg)
	if err != nil {
		return nil, fmt.Errorf("building rest config for cluster %q: %w", name, err)
	}

	// Create the client
	k8sClient, err := client.New(restConfig, client.Options{Scheme: r.scheme})
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client for cluster %q: %w", name, err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes clientset for cluster %q: %w", name, err)
	}

	clusterClient := &ClusterClient{
		Name:      name,
		APIServer: clusterCfg.APIServer,
		Client:    k8sClient,
		Clientset: clientset,
	}

	r.clusters[name] = clusterClient
	return clusterClient, nil
}

func (r *clusterRegistry) buildRestConfig(cfg *ClusterConfig) (*rest.Config, error) {
	// Strategy 1: Kubeconfig file
	if cfg.Kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags(cfg.APIServer, cfg.Kubeconfig)
	}

	// Strategy 2: ServiceAccount token + CA cert
	if cfg.ServiceAccountToken != "" {
		token, err := os.ReadFile(cfg.ServiceAccountToken)
		if err != nil {
			return nil, fmt.Errorf("reading service account token: %w", err)
		}

		var caData []byte
		if cfg.CACert != "" {
			caData, err = os.ReadFile(cfg.CACert)
			if err != nil {
				return nil, fmt.Errorf("reading CA cert: %w", err)
			}
		}

		return &rest.Config{
			Host:        cfg.APIServer,
			BearerToken: string(token),
			TLSClientConfig: rest.TLSClientConfig{
				CAData: caData,
			},
		}, nil
	}

	// Strategy 3: In-cluster config (for the local cluster)
	if cfg.Name == "local" || cfg.APIServer == "" {
		return rest.InClusterConfig()
	}

	return nil, fmt.Errorf("no authentication method configured for cluster %q", cfg.Name)
}
