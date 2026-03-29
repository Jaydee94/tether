package audit

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Backend is the interface for audit storage backends.
type Backend interface {
	Write(ctx context.Context, sessionID string, data []byte) error
	Read(ctx context.Context, sessionID string) ([]byte, error)
}

// LocalBackend writes audit recordings to the local filesystem.
type LocalBackend struct {
	Dir string
}

// NewLocalBackend creates a LocalBackend rooted at dir.
func NewLocalBackend(dir string) (*LocalBackend, error) {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("creating audit dir %q: %w", dir, err)
	}
	return &LocalBackend{Dir: dir}, nil
}

func (b *LocalBackend) Write(_ context.Context, sessionID string, data []byte) error {
	path := filepath.Join(b.Dir, sessionID+".cast")
	return os.WriteFile(path, data, 0640)
}

func (b *LocalBackend) Read(_ context.Context, sessionID string) ([]byte, error) {
	path := filepath.Join(b.Dir, sessionID+".cast")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading session %q: %w", sessionID, err)
	}
	return data, nil
}

// S3Backend writes audit recordings to an AWS S3 bucket.
type S3Backend struct {
	client *s3.Client
	bucket string
	prefix string
}

// NewS3Backend creates an S3Backend using the default AWS credential chain.
func NewS3Backend(ctx context.Context, bucket, prefix string) (*S3Backend, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	return &S3Backend{
		client: s3.NewFromConfig(cfg),
		bucket: bucket,
		prefix: prefix,
	}, nil
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

// ElasticBackend indexes audit recordings to Elasticsearch using its HTTP API.
type ElasticBackend struct {
	endpoint   string
	index      string
	httpClient *http.Client
}

// NewElasticBackend creates an ElasticBackend targeting the given endpoint and index.
func NewElasticBackend(endpoint, index string) *ElasticBackend {
	return &ElasticBackend{
		endpoint:   endpoint,
		index:      index,
		httpClient: &http.Client{},
	}
}

func (b *ElasticBackend) Write(ctx context.Context, sessionID string, data []byte) error {
	url := fmt.Sprintf("%s/%s/_doc/%s", b.endpoint, b.index, sessionID)
	body := fmt.Sprintf(`{"sessionID":%q,"cast":%q}`, sessionID, string(data))

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewBufferString(body))
	if err != nil {
		return fmt.Errorf("building Elasticsearch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

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
