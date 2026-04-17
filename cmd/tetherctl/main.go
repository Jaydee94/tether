package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
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
	rootCmd.AddCommand(newRequestCmd(), newLoginCmd(), newPlaybackCmd(), newListCmd(), newRevokeCmd(), newApproveCmd(), newDenyCmd())
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

var invalidNameChars = regexp.MustCompile(`[^a-z0-9-]+`)
var repeatedDash = regexp.MustCompile(`-+`)

// defaultTokenNamespace is where the operator stores session-token Secrets.
func defaultTokenNamespace() string {
	if ns := os.Getenv("TETHER_NAMESPACE"); ns != "" {
		return ns
	}
	return "tether-system"
}

func newRequestCmd() *cobra.Command {
	var (
		role      string
		duration  string
		reason    string
		name      string
		namespace string
		cluster   string
	)

	cmd := &cobra.Command{
		Use:   "request",
		Short: "Request a new TetherLease for privileged access",
		Example: `  # Request cluster-admin access for 30 minutes
  tetherctl request --role cluster-admin --for 30m --reason "investigating outage"

  # Request access to a specific cluster
  tetherctl request --role cluster-admin --for 30m --cluster prod-us-west-2 --reason "prod incident"

  # Request namespace-scoped access (creates a RoleBinding in 'dev')
  tetherctl request --role developer --for 1h --namespace dev --reason "deploying hotfix"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if role == "" {
				return fmt.Errorf("--role is required")
			}
			if duration == "" {
				return fmt.Errorf("--for is required")
			}
			leaseDuration, err := time.ParseDuration(duration)
			if err != nil {
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
				name = autoLeaseName(user, time.Now())
			}

			spec := map[string]interface{}{
				"user":     user,
				"role":     role,
				"duration": duration,
				"reason":   reason,
			}
			if namespace != "" {
				spec["namespace"] = namespace
			}
			if cluster != "" {
				spec["cluster"] = cluster
			}

			lease := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "tether.dev/v1alpha1",
					"kind":       "TetherLease",
					"metadata": map[string]interface{}{
						"name": name,
					},
					"spec": spec,
				},
			}

			created, err := dynClient.Resource(tetherLeaseGVR).Create(context.Background(), lease, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("creating TetherLease: %w", err)
			}

			fmt.Printf("TetherLease %q created.\n", created.GetName())
			fmt.Printf("User: %s | Role: %s | Duration: %s\n", user, role, duration)
			if cluster != "" {
				fmt.Printf("Cluster: %s\n", cluster)
			}
			if namespace != "" {
				fmt.Printf("Scope: namespace %q (RoleBinding)\n", namespace)
			} else {
				fmt.Println("Scope: cluster-wide (ClusterRoleBinding)")
			}
			expectedExpiry := time.Now().Add(leaseDuration)
			fmt.Printf("Expected expiry: %s (%s)\n", expectedExpiry.Format(time.RFC3339), relativeFrom(time.Now(), expectedExpiry))
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
	cmd.Flags().StringVar(&namespace, "namespace", "", "Namespace to scope the binding to (omit for cluster-wide ClusterRoleBinding)")
	cmd.Flags().StringVar(&cluster, "cluster", "", "Target cluster name (omit for default/local cluster)")
	return cmd
}

func newLoginCmd() *cobra.Command {
	var (
		leaseName          string
		proxyAddr          string
		proxyToken         string
		insecureSkipVerify bool
		tokenNamespace     string
	)

	cmd := &cobra.Command{
		Use:     "login",
		Short:   "Configure kubeconfig to route through the Tether proxy",
		Example: `  tetherctl login --lease alice-1234567890`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if leaseName == "" {
				return fmt.Errorf("--lease is required")
			}
			if isPlaceholderLeaseName(leaseName) {
				return fmt.Errorf("--lease appears to be a placeholder (%q). Use a real lease name, e.g. demo-kind-tether-dev-setup", leaseName)
			}

			// If no token was provided, try to fetch it from the k8s Secret created by the operator.
			if proxyToken == "" {
				fetched, err := fetchTokenFromSecret(leaseName, tokenNamespace)
				if err != nil {
					return fmt.Errorf("could not auto-fetch token for lease %q (pass --token to specify manually): %w", leaseName, err)
				}
				proxyToken = fetched
				fmt.Printf("Token auto-fetched from Kubernetes Secret (namespace: %s).\n", tokenNamespace)
			}

			if proxyToken == "" {
				return fmt.Errorf("--token is required (set --token or TETHER_TOKEN, or ensure the lease is Active so the token can be auto-fetched)")
			}

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
				Token: proxyToken,
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
			fmt.Println("\nYour kubectl commands are now routed through the Tether proxy.")
			fmt.Println("The session will be recorded for audit purposes.")
			return nil
		},
	}

	cmd.Flags().StringVar(&leaseName, "lease", "", "Name of the TetherLease to activate (required)")
	cmd.Flags().StringVar(&proxyAddr, "proxy", "https://localhost:8443", "Address of the Tether proxy")
	cmd.Flags().StringVar(&proxyToken, "token", os.Getenv("TETHER_TOKEN"), "Proxy token (auto-fetched from k8s if omitted; or set TETHER_TOKEN)")
	cmd.Flags().BoolVar(&insecureSkipVerify, "insecure-skip-tls-verify", false, "Skip TLS certificate verification of the proxy (dev only)")
	cmd.Flags().StringVar(&tokenNamespace, "token-namespace", defaultTokenNamespace(), "Namespace where the operator stores session-token Secrets")
	return cmd
}

// fetchTokenFromSecret reads the session token from the k8s Secret that the operator created.
func fetchTokenFromSecret(leaseName, namespace string) (string, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return "", fmt.Errorf("loading kubeconfig: %w", err)
	}
	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", fmt.Errorf("creating kubernetes client: %w", err)
	}
	secretName := "tether-token-" + leaseName
	secret, err := kubeClient.CoreV1().Secrets(namespace).Get(context.Background(), secretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading secret %q in namespace %q: %w", secretName, namespace, err)
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		return "", fmt.Errorf("secret %q has no token data", secretName)
	}
	return token, nil
}

func newListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List all TetherLeases",
		Example: `  tetherctl list`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
			if err != nil {
				return fmt.Errorf("loading kubeconfig: %w", err)
			}
			dynClient, err := dynamic.NewForConfig(cfg)
			if err != nil {
				return fmt.Errorf("creating dynamic client: %w", err)
			}

			list, err := dynClient.Resource(tetherLeaseGVR).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				return fmt.Errorf("listing TetherLeases: %w", err)
			}

			if len(list.Items) == 0 {
				fmt.Println("No TetherLeases found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tUSER\tROLE\tPHASE\tEXPIRES\tREASON")
			fmt.Fprintln(w, "----\t----\t----\t-----\t-------\t------")
			for i := range list.Items {
				item := &list.Items[i]
				name := item.GetName()
				user, _, _ := unstructured.NestedString(item.Object, "spec", "user")
				role, _, _ := unstructured.NestedString(item.Object, "spec", "role")
				reason, _, _ := unstructured.NestedString(item.Object, "spec", "reason")
				phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
				expiresAt, _, _ := unstructured.NestedString(item.Object, "status", "expiresAt")

				if phase == "" {
					phase = "Pending"
				}
				if reason == "" {
					reason = "-"
				}
				expiry := "-"
				if expiresAt != "" {
					if t, err := time.Parse(time.RFC3339, expiresAt); err == nil {
						if time.Now().After(t) {
							expiry = "expired"
						} else {
							expiry = relativeFrom(time.Now(), t)
						}
					} else {
						expiry = expiresAt
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", name, user, role, phase, expiry, reason)
			}
			return w.Flush()
		},
	}
	return cmd
}

func newRevokeCmd() *cobra.Command {
	var leaseName string

	cmd := &cobra.Command{
		Use:     "revoke",
		Short:   "Revoke a TetherLease immediately",
		Example: `  tetherctl revoke --lease alice-1234567890`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if leaseName == "" {
				return fmt.Errorf("--lease is required")
			}

			cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
			if err != nil {
				return fmt.Errorf("loading kubeconfig: %w", err)
			}
			dynClient, err := dynamic.NewForConfig(cfg)
			if err != nil {
				return fmt.Errorf("creating dynamic client: %w", err)
			}

			// Fetch the current lease so we can patch its status.
			lease, err := dynClient.Resource(tetherLeaseGVR).Get(context.Background(), leaseName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("getting TetherLease %q: %w", leaseName, err)
			}

			phase, _, _ := unstructured.NestedString(lease.Object, "status", "phase")
			if phase == "Expired" || phase == "Revoked" {
				fmt.Printf("Lease %q is already %s.\n", leaseName, phase)
				return nil
			}

			// Set phase to Revoked via status subresource.
			if err := unstructured.SetNestedField(lease.Object, "Revoked", "status", "phase"); err != nil {
				return fmt.Errorf("setting phase: %w", err)
			}
			if _, err := dynClient.Resource(tetherLeaseGVR).UpdateStatus(context.Background(), lease, metav1.UpdateOptions{}); err != nil {
				return fmt.Errorf("revoking lease %q: %w", leaseName, err)
			}

			fmt.Printf("Lease %q has been revoked.\n", leaseName)
			fmt.Println("The operator will delete the ClusterRoleBinding and session token.")
			return nil
		},
	}

	cmd.Flags().StringVar(&leaseName, "lease", "", "Name of the TetherLease to revoke (required)")
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
			if isPlaceholderLeaseName(leaseName) {
				return fmt.Errorf("--lease appears to be a placeholder (%q). In local dev mode, try: --lease dev-session", leaseName)
			}

			backend, err := audit.NewLocalBackend(auditDir)
			if err != nil {
				return fmt.Errorf("opening audit backend: %w", err)
			}

			data, err := backend.Read(context.Background(), leaseName)
			if err != nil {
				if os.IsNotExist(err) {
					return playbackNotFoundError(leaseName, auditDir)
				}
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

	recordedAt := time.Unix(hdr.Timestamp, 0)
	fmt.Printf("Playing back: %s (recorded at %s, %s)\n\n",
		hdr.Title, recordedAt.Format(time.RFC3339), relativeFrom(time.Now(), recordedAt))

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
			fmt.Print(formatPlaybackOutput(eventData))
			lastTime = t
		}
	}
	fmt.Println()
	return nil
}

func formatPlaybackOutput(eventData string) string {
	trimmed := strings.TrimSpace(eventData)
	if strings.HasPrefix(trimmed, "{") && strings.Contains(trimmed, "\"kind\"") {
		tableText, ok := renderKubeTableJSON(trimmed)
		if ok {
			if !strings.HasSuffix(tableText, "\n") {
				tableText += "\n"
			}
			return tableText
		}
	}
	return eventData
}

func renderKubeTableJSON(raw string) (string, bool) {
	type colDef struct {
		Name string `json:"name"`
	}
	type row struct {
		Cells []interface{} `json:"cells"`
	}
	type kubeTable struct {
		Kind              string   `json:"kind"`
		ColumnDefinitions []colDef `json:"columnDefinitions"`
		Rows              []row    `json:"rows"`
	}

	var table kubeTable
	if err := json.Unmarshal([]byte(raw), &table); err != nil {
		return "", false
	}
	if table.Kind != "Table" || len(table.ColumnDefinitions) == 0 {
		return "", false
	}

	headers := make([]string, 0, len(table.ColumnDefinitions))
	widths := make([]int, 0, len(table.ColumnDefinitions))
	for _, c := range table.ColumnDefinitions {
		headers = append(headers, c.Name)
		widths = append(widths, len(c.Name))
	}

	rows := make([][]string, 0, len(table.Rows))
	for _, r := range table.Rows {
		rowVals := make([]string, len(headers))
		for i := range headers {
			if i < len(r.Cells) {
				rowVals[i] = fmt.Sprint(r.Cells[i])
			}
			if len(rowVals[i]) > widths[i] {
				widths[i] = len(rowVals[i])
			}
		}
		rows = append(rows, rowVals)
	}

	var b strings.Builder
	writeRow := func(vals []string) {
		for i, v := range vals {
			if i > 0 {
				b.WriteString("  ")
			}
			b.WriteString(v)
			if pad := widths[i] - len(v); pad > 0 {
				b.WriteString(strings.Repeat(" ", pad))
			}
		}
		b.WriteByte('\n')
	}

	writeRow(headers)
	for _, r := range rows {
		writeRow(r)
	}

	return b.String(), true
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

func relativeFrom(now, target time.Time) string {
	delta := target.Sub(now).Round(time.Second)
	if delta < 0 {
		return fmt.Sprintf("%s ago", (-delta).String())
	}
	if delta == 0 {
		return "now"
	}
	return fmt.Sprintf("in %s", delta.String())
}

func isPlaceholderLeaseName(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	normalized = strings.Trim(normalized, "<>")
	normalized = strings.ReplaceAll(normalized, "-", "")
	normalized = strings.ReplaceAll(normalized, "_", "")
	normalized = strings.ReplaceAll(normalized, " ", "")

	switch normalized {
	case "leasename", "yourleasename", "lease":
		return true
	default:
		return false
	}
}

func playbackNotFoundError(leaseName, auditDir string) error {
	sessions, err := listRecordedSessions(auditDir)
	if err != nil {
		return fmt.Errorf("reading session %q: no recording found in %s", leaseName, auditDir)
	}

	if len(sessions) == 0 {
		return fmt.Errorf("reading session %q: no recording found in %s (no .cast files present)", leaseName, auditDir)
	}

	hint := strings.Join(sessions, ", ")
	if containsString(sessions, "dev-session") {
		return fmt.Errorf("reading session %q: no recording found in %s. Available sessions: %s. In local dev mode, use --lease dev-session", leaseName, auditDir, hint)
	}
	return fmt.Errorf("reading session %q: no recording found in %s. Available sessions: %s", leaseName, auditDir, hint)
}

func listRecordedSessions(auditDir string) ([]string, error) {
	entries, err := os.ReadDir(auditDir)
	if err != nil {
		return nil, err
	}

	var sessions []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".cast") {
			sessions = append(sessions, strings.TrimSuffix(name, ".cast"))
		}
	}
	return sessions, nil
}

func containsString(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

func newApproveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "approve <lease-name>",
		Short:   "Approve a TetherLease pending approval",
		Example: `  tetherctl approve alice-1234567890`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			leaseName := args[0]
			return patchLeaseApproval(leaseName, true)
		},
	}
	return cmd
}

func newDenyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "deny <lease-name>",
		Short:   "Deny a TetherLease pending approval",
		Example: `  tetherctl deny alice-1234567890`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			leaseName := args[0]
			return patchLeaseApproval(leaseName, false)
		},
	}
	return cmd
}

func patchLeaseApproval(leaseName string, approve bool) error {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	actor := currentUser(kubeconfig)

	var statusPatch map[string]interface{}
	if approve {
		statusPatch = map[string]interface{}{
			"status": map[string]interface{}{
				"phase":      "Pending",
				"approvedBy": actor,
			},
		}
	} else {
		statusPatch = map[string]interface{}{
			"status": map[string]interface{}{
				"phase":    "Denied",
				"deniedBy": actor,
			},
		}
	}

	patchBytes, err := json.Marshal(statusPatch)
	if err != nil {
		return fmt.Errorf("marshaling patch: %w", err)
	}

	_, err = dynClient.Resource(tetherLeaseGVR).Patch(
		context.Background(),
		leaseName,
		types.MergePatchType,
		patchBytes,
		metav1.PatchOptions{},
		"status",
	)
	if err != nil {
		action := "approving"
		if !approve {
			action = "denying"
		}
		return fmt.Errorf("%s TetherLease %q: %w", action, leaseName, err)
	}

	if approve {
		fmt.Printf("TetherLease %q approved by %s.\n", leaseName, actor)
		fmt.Println("The operator will now activate the lease.")
	} else {
		fmt.Printf("TetherLease %q denied by %s.\n", leaseName, actor)
	}
	return nil
}

func autoLeaseName(user string, now time.Time) string {
	base := strings.ToLower(strings.TrimSpace(user))
	base = invalidNameChars.ReplaceAllString(base, "-")
	base = repeatedDash.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "user"
	}

	// Keep enough room for separator + Unix timestamp suffix.
	const maxBaseLen = 240
	if len(base) > maxBaseLen {
		base = base[:maxBaseLen]
		base = strings.Trim(base, "-")
		if base == "" {
			base = "user"
		}
	}

	return fmt.Sprintf("%s-%d", base, now.Unix())
}
