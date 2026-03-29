package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Jaydee94/tether/pkg/audit"
)

type castHeader struct {
	Version   int    `json:"version"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Timestamp int64  `json:"timestamp"`
	Title     string `json:"title"`
}

type castEvent struct {
	Time float64
	Type string
	Data string
}

func (e castEvent) MarshalJSON() ([]byte, error) {
	return json.Marshal([]interface{}{e.Time, e.Type, e.Data})
}

// Recorder captures session output and writes an Asciinema v2 .cast file.
type Recorder struct {
	sessionID string
	backend   audit.Backend
	title     string

	mu        sync.Mutex
	startTime time.Time
	buf       bytes.Buffer
}

// NewRecorder creates a new Recorder for the given session.
func NewRecorder(sessionID string, backend audit.Backend, title string) *Recorder {
	return &Recorder{
		sessionID: sessionID,
		backend:   backend,
		title:     title,
	}
}

// Start writes the Asciinema header and records the start time.
func (r *Recorder) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.startTime = time.Now()
	hdr := castHeader{
		Version:   2,
		Width:     220,
		Height:    50,
		Timestamp: r.startTime.Unix(),
		Title:     r.title,
	}
	b, err := json.Marshal(hdr)
	if err != nil {
		return fmt.Errorf("marshalling cast header: %w", err)
	}
	r.buf.Write(b)
	r.buf.WriteByte('\n')
	return nil
}

// RecordOutput records a chunk of output data with the current time offset.
func (r *Recorder) RecordOutput(data []byte) {
	if len(data) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	offset := time.Since(r.startTime).Seconds()
	event := castEvent{Time: offset, Type: "o", Data: string(data)}
	b, err := json.Marshal(event)
	if err != nil {
		ctrllog.Log.WithName("recorder").Error(err, "Failed to marshal cast event", "sessionID", r.sessionID)
		return
	}
	r.buf.Write(b)
	r.buf.WriteByte('\n')
}

// Finish saves the completed .cast file to the audit backend.
func (r *Recorder) Finish(ctx context.Context) error {
	r.mu.Lock()
	data := make([]byte, r.buf.Len())
	copy(data, r.buf.Bytes())
	r.mu.Unlock()

	return r.backend.Write(ctx, r.sessionID, data)
}

// RecordingWriter wraps an io.Writer and tees data to a Recorder.
type RecordingWriter struct {
	inner    io.Writer
	recorder *Recorder
}

// NewRecordingWriter creates a writer that tees to both inner and recorder.
func NewRecordingWriter(inner io.Writer, recorder *Recorder) *RecordingWriter {
	return &RecordingWriter{inner: inner, recorder: recorder}
}

func (w *RecordingWriter) Write(p []byte) (int, error) {
	w.recorder.RecordOutput(p)
	return w.inner.Write(p)
}
