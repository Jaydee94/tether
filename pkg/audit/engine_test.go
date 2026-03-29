package audit

import (
	"context"
	"testing"
)

func TestLocalBackend_WriteRead(t *testing.T) {
	dir := t.TempDir()
	b, err := NewLocalBackend(dir)
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}

	ctx := context.Background()
	data := []byte(`{"version":2,"width":220,"height":50,"timestamp":1704067200,"title":"test"}` + "\n" +
		`[0.1,"o","hello\r\n"]` + "\n")

	if err := b.Write(ctx, "sess-001", data); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := b.Read(ctx, "sess-001")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if string(got) != string(data) {
		t.Errorf("data mismatch\ngot:  %q\nwant: %q", got, data)
	}
}

func TestLocalBackend_ReadNotFound(t *testing.T) {
	b, err := NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	_, err = b.Read(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}
