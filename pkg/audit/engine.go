package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Backend is the interface for audit storage backends.
type Backend interface {
	Write(ctx context.Context, sessionID string, data []byte) error
	Read(ctx context.Context, sessionID string) ([]byte, error)
}

// ---------------------------------------------------------------------------
// LocalBackend
// ---------------------------------------------------------------------------

// LocalBackendConfig holds optional tuning parameters for LocalBackend.
type LocalBackendConfig struct {
	// MaxFileSizeBytes is the maximum size a single .cast file may grow to
	// before it is rotated. When a write would cause the file to exceed this
	// limit the existing file is renamed to <sessionID>-<n>.cast and a fresh
	// file is started. Set to 0 (the default) to disable rotation.
	MaxFileSizeBytes int64
}

// LocalBackend writes audit recordings to the local filesystem.
type LocalBackend struct {
	Dir    string
	cfg    LocalBackendConfig
	mu     sync.Mutex // protects rotation counter map
	rotIdx map[string]int
}

// NewLocalBackend creates a LocalBackend rooted at dir with default config
// (no rotation).
func NewLocalBackend(dir string) (*LocalBackend, error) {
	return NewLocalBackendWithConfig(dir, LocalBackendConfig{})
}

// NewLocalBackendWithConfig creates a LocalBackend with the supplied config.
func NewLocalBackendWithConfig(dir string, cfg LocalBackendConfig) (*LocalBackend, error) {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("creating audit dir %q: %w", dir, err)
	}
	return &LocalBackend{Dir: dir, cfg: cfg, rotIdx: make(map[string]int)}, nil
}

func (b *LocalBackend) activePath(sessionID string) string {
	return filepath.Join(b.Dir, sessionID+".cast")
}

func (b *LocalBackend) Write(_ context.Context, sessionID string, data []byte) error {
	path := b.activePath(sessionID)

	if b.cfg.MaxFileSizeBytes > 0 {
		b.mu.Lock()
		defer b.mu.Unlock()

		if info, err := os.Stat(path); err == nil {
			if info.Size()+int64(len(data)) > b.cfg.MaxFileSizeBytes {
				b.rotIdx[sessionID]++
				rotated := filepath.Join(b.Dir, fmt.Sprintf("%s-%d.cast", sessionID, b.rotIdx[sessionID]))
				if err := os.Rename(path, rotated); err != nil {
					return fmt.Errorf("rotating audit file for session %q: %w", sessionID, err)
				}
			}
		}
	}

	return os.WriteFile(path, data, 0644)
}

func (b *LocalBackend) Read(_ context.Context, sessionID string) ([]byte, error) {
	path := b.activePath(sessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading session %q: %w", sessionID, err)
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// S3Backend
// ---------------------------------------------------------------------------

// S3BackendConfig holds optional parameters for S3Backend.
type S3BackendConfig struct {
	// ValidateOnStartup when true causes NewS3BackendWithConfig to perform a
	// HeadBucket call so misconfigured buckets are caught at startup rather
	// than at first write.
	ValidateOnStartup bool
}

// S3Backend writes audit recordings to an AWS S3 bucket.
type S3Backend struct {
	client *s3.Client
	bucket string
	prefix string
}

// NewS3Backend creates an S3Backend using the default AWS credential chain.
// It does NOT validate bucket access at startup; use NewS3BackendWithConfig
// with ValidateOnStartup: true for production.
func NewS3Backend(ctx context.Context, bucket, prefix string) (*S3Backend, error) {
	return NewS3BackendWithConfig(ctx, bucket, prefix, S3BackendConfig{})
}

// NewS3BackendWithConfig creates an S3Backend and optionally validates that
// the bucket is reachable (HeadBucket) before returning.
func NewS3BackendWithConfig(ctx context.Context, bucket, prefix string, cfg S3BackendConfig) (*S3Backend, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	b := &S3Backend{
		client: s3.NewFromConfig(awsCfg),
		bucket: bucket,
		prefix: prefix,
	}

	if cfg.ValidateOnStartup {
		if _, err := b.client.HeadBucket(ctx, &s3.HeadBucketInput{
			Bucket: aws.String(bucket),
		}); err != nil {
			return nil, fmt.Errorf("validating S3 bucket %q at startup: %w", bucket, err)
		}
	}

	return b, nil
}

func (b *S3Backend) s3Key(sessionID string) string {
	if b.prefix != "" {
		return b.prefix + "/" + sessionID + ".cast"
	}
	return sessionID + ".cast"
}

func (b *S3Backend) Write(ctx context.Context, sessionID string, data []byte) error {
	key := b.s3Key(sessionID)
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(b.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/x-asciicast"),
	})
	if err != nil {
		return fmt.Errorf("uploading session %q to S3: %w", sessionID, err)
	}
	return nil
}

func (b *S3Backend) Read(ctx context.Context, sessionID string) ([]byte, error) {
	key := b.s3Key(sessionID)
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("downloading session %q from S3: %w", sessionID, err)
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

// ---------------------------------------------------------------------------
// ElasticBackend
// ---------------------------------------------------------------------------

// ElasticAuthConfig holds authentication credentials for Elasticsearch.
// Exactly one of APIKey or (Username + Password) should be set; if both are
// provided, APIKey takes precedence.
type ElasticAuthConfig struct {
	// APIKey is sent as "Authorization: ApiKey <value>".
	APIKey string

	// Username and Password are used for HTTP Basic authentication.
	Username string
	Password string
}

// ElasticBackendConfig holds optional parameters for ElasticBackend.
type ElasticBackendConfig struct {
	Auth ElasticAuthConfig
}

// ElasticBackend indexes audit recordings to Elasticsearch using its HTTP API.
type ElasticBackend struct {
	endpoint   string
	index      string
	cfg        ElasticBackendConfig
	httpClient *http.Client
}

// NewElasticBackend creates an ElasticBackend targeting the given endpoint and
// index with no authentication. Use NewElasticBackendWithConfig for production
// deployments that require auth.
func NewElasticBackend(endpoint, index string) *ElasticBackend {
	return NewElasticBackendWithConfig(endpoint, index, ElasticBackendConfig{})
}

// NewElasticBackendWithConfig creates an ElasticBackend with optional auth.
func NewElasticBackendWithConfig(endpoint, index string, cfg ElasticBackendConfig) *ElasticBackend {
	return &ElasticBackend{
		endpoint:   endpoint,
		index:      index,
		cfg:        cfg,
		httpClient: &http.Client{},
	}
}

// applyAuth attaches the configured authentication headers to the request.
func (b *ElasticBackend) applyAuth(req *http.Request) {
	auth := b.cfg.Auth
	switch {
	case auth.APIKey != "":
		req.Header.Set("Authorization", "ApiKey "+auth.APIKey)
	case auth.Username != "":
		req.SetBasicAuth(auth.Username, auth.Password)
	}
}

func (b *ElasticBackend) Write(ctx context.Context, sessionID string, data []byte) error {
	url := fmt.Sprintf("%s/%s/_doc/%s", b.endpoint, b.index, sessionID)

	doc := struct {
		SessionID string `json:"sessionID"`
		Cast      string `json:"cast"`
	}{SessionID: sessionID, Cast: string(data)}

	bodyBytes, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshalling Elasticsearch document for session %q: %w", sessionID, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("building Elasticsearch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	b.applyAuth(req)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("indexing session %q to Elasticsearch: %w", sessionID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("elasticsearch returned status %d for session %q", resp.StatusCode, sessionID)
	}
	return nil
}

func (b *ElasticBackend) Read(ctx context.Context, sessionID string) ([]byte, error) {
	url := fmt.Sprintf("%s/%s/_doc/%s", b.endpoint, b.index, sessionID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building Elasticsearch request: %w", err)
	}
	b.applyAuth(req)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching session %q from Elasticsearch: %w", sessionID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("session %q not found", sessionID)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("elasticsearch returned status %d for session %q", resp.StatusCode, sessionID)
	}
	return io.ReadAll(resp.Body)
}
