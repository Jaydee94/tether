package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/Jaydee94/tether/pkg/audit"
)

var (
	kubeconfig string
	rootCmd    = &cobra.Command{
		Use:   "tetherctl",
		Short: "tetherctl manages Tether privileged access leases",
		Long: `tetherctl is the CLI for the Tether Kubernetes privileged access management system.
It allows you to request time-limited access, configure kubeconfig, and play back session recordings.`,
	}
)

func main() {
	rootCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", defaultKubeconfig(), "Path to kubeconfig file.")
	rootCmd.AddCommand(newRequestCmd(), newLoginCmd(), newPlaybackCmd())
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func defaultKubeconfig() string {
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		return kc
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kube", "config")
}

var tetherLeaseGVR = schema.GroupVersionResource{
	Group:    "tether.dev",
	Version:  "v1alpha1",
	Resource: "tetherleases",
}

func newRequestCmd() *cobra.Command {
	var (
		role     string
		duration string
		reason   string
		name     string
	)

	cmd := &cobra.Command{
		Use:   "request",
		Short: "Request a new TetherLease for privileged access",
		Example: `  # Request cluster-admin access for 30 minutes
  tetherctl request --role cluster-admin --for 30m --reason "investigating outage"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if role == "" {
				return fmt.Errorf("--role is required")
			}
			if duration == "" {
				return fmt.Errorf("--for is required")
			}
			if _, err := time.ParseDuration(duration); err != nil {
				return fmt.Errorf("invalid duration %q: %w", duration, err)
			}

			cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
			if err != nil {
				return fmt.Errorf("loading kubeconfig: %w", err)
			}

			dynClient, err := dynamic.NewForConfig(cfg)
			if err != nil {
				return fmt.Errorf("creating dynamic client: %w", err)
			}

			user := currentUser(kubeconfig)
			if name == "" {
				name = fmt.Sprintf("%s-%d", user, time.Now().Unix())
			}

			lease := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "tether.dev/v1alpha1",
					"kind":       "TetherLease",
					"metadata": map[string]interface{}{
						"name": name,
					},
					"spec": map[string]interface{}{
						"user":     user,
						"role":     role,
						"duration": duration,
						"reason":   reason,
					},
				},
			}

			created, err := dynClient.Resource(tetherLeaseGVR).Create(context.Background(), lease, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("creating TetherLease: %w", err)
			}

			fmt.Printf("TetherLease %q created.\n", created.GetName())
			fmt.Printf("User: %s | Role: %s | Duration: %s\n", user, role, duration)
			if reason != "" {
				fmt.Printf("Reason: %s\n", reason)
			}
			fmt.Println("\nRun `tetherctl login --lease", created.GetName(), "` to activate your session.")
			return nil
		},
	}

	cmd.Flags().StringVar(&role, "role", "", "ClusterRole to request access to (required)")
	cmd.Flags().StringVar(&duration, "for", "", "Duration for the lease, e.g. 30m, 1h (required)")
	cmd.Flags().StringVar(&reason, "reason", "", "Human-readable reason for the access request")
	cmd.Flags().StringVar(&name, "name", "", "Name for the TetherLease (defaults to <user>-<timestamp>)")
	return cmd
}

func newLoginCmd() *cobra.Command {
	var (
		leaseName          string
		proxyAddr          string
		insecureSkipVerify bool
	)

	cmd := &cobra.Command{
		Use:     "login",
		Short:   "Configure kubeconfig to route through the Tether proxy",
		Example: `  tetherctl login --lease alice-1234567890`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if leaseName == "" {
				return fmt.Errorf("--lease is required")
			}

			token := fmt.Sprintf("tether-%s-%d", leaseName, time.Now().UnixNano())
			contextName := "tether-" + leaseName

			rules := clientcmd.NewDefaultClientConfigLoadingRules()
			rules.ExplicitPath = kubeconfig
			rawCfg, err := rules.Load()
			if err != nil || rawCfg == nil {
				rawCfg = clientcmdapi.NewConfig()
			}

			rawCfg.Clusters[contextName] = &clientcmdapi.Cluster{
				Server:                proxyAddr,
				InsecureSkipTLSVerify: insecureSkipVerify,
			}
			rawCfg.AuthInfos[contextName] = &clientcmdapi.AuthInfo{
				Token: token,
			}
			rawCfg.Contexts[contextName] = &clientcmdapi.Context{
				Cluster:  contextName,
				AuthInfo: contextName,
			}
			rawCfg.CurrentContext = contextName

			if err := clientcmd.WriteToFile(*rawCfg, kubeconfig); err != nil {
				return fmt.Errorf("writing kubeconfig: %w", err)
			}

			fmt.Printf("Kubeconfig updated. Active context: %q\n", contextName)
			fmt.Printf("X-Tether-Token: %s\n", token)
			fmt.Println("\nYour kubectl commands are now routed through the Tether proxy.")
			fmt.Println("The session will be recorded for audit purposes.")
			return nil
		},
	}

	cmd.Flags().StringVar(&leaseName, "lease", "", "Name of the TetherLease to activate (required)")
	cmd.Flags().StringVar(&proxyAddr, "proxy", "https://localhost:8443", "Address of the Tether proxy")
	cmd.Flags().BoolVar(&insecureSkipVerify, "insecure-skip-tls-verify", false, "Skip TLS certificate verification of the proxy (dev only)")
	return cmd
}

func newPlaybackCmd() *cobra.Command {
	var (
		leaseName string
		auditDir  string
	)

	cmd := &cobra.Command{
		Use:     "playback",
		Short:   "Play back a recorded session from the audit backend",
		Example: `  tetherctl playback --lease alice-1234567890`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if leaseName == "" {
				return fmt.Errorf("--lease is required")
			}

			backend, err := audit.NewLocalBackend(auditDir)
			if err != nil {
				return fmt.Errorf("opening audit backend: %w", err)
			}

			data, err := backend.Read(context.Background(), leaseName)
			if err != nil {
				return fmt.Errorf("reading session %q: %w", leaseName, err)
			}

			return playbackCast(data)
		},
	}

	cmd.Flags().StringVar(&leaseName, "lease", "", "Name of the TetherLease (used as session ID) (required)")
	cmd.Flags().StringVar(&auditDir, "audit-dir", "/var/tether/audit", "Directory containing audit recordings")
	return cmd
}

func playbackCast(data []byte) error {
	lines := splitLines(data)
	if len(lines) == 0 {
		return fmt.Errorf("empty recording")
	}

	var hdr struct {
		Version   int    `json:"version"`
		Title     string `json:"title"`
		Timestamp int64  `json:"timestamp"`
	}
	if err := json.Unmarshal(lines[0], &hdr); err != nil {
		return fmt.Errorf("parsing cast header: %w", err)
	}

	fmt.Printf("Playing back: %s (recorded at %s)\n\n",
		hdr.Title, time.Unix(hdr.Timestamp, 0).Format(time.RFC3339))

	var lastTime float64
	for _, line := range lines[1:] {
		if len(line) == 0 {
			continue
		}
		var event []json.RawMessage
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		if len(event) != 3 {
			continue
		}

		var t float64
		var eventType, eventData string
		if err := json.Unmarshal(event[0], &t); err != nil {
			continue
		}
		if err := json.Unmarshal(event[1], &eventType); err != nil {
			continue
		}
		if err := json.Unmarshal(event[2], &eventData); err != nil {
			continue
		}

		if eventType == "o" {
			delay := t - lastTime
			if delay > 0 && delay < 5 {
				time.Sleep(time.Duration(delay * float64(time.Second)))
			}
			fmt.Print(eventData)
			lastTime = t
		}
	}
	fmt.Println()
	return nil
}

func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				lines = append(lines, data[start:i])
			}
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

func currentUser(kubeconfigPath string) string {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfigPath
	cfg, err := rules.Load()
	if err != nil {
		return envUser()
	}
	ctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok || ctx == nil {
		return envUser()
	}
	if ctx.AuthInfo != "" {
		return ctx.AuthInfo
	}
	return envUser()
}

func envUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}
