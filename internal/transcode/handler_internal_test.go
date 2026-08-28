package transcode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitForFileIgnoresEmptyPlaceholder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "segment_00000.ts")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if waitForFile(context.Background(), path, 250*time.Millisecond) {
		t.Fatal("empty placeholder should not be treated as ready")
	}
}

func TestWaitForFileAcceptsNonEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "segment_00000.ts")
	go func() {
		time.Sleep(40 * time.Millisecond)
		if err := os.WriteFile(path, []byte("tsdata"), 0o644); err != nil {
			t.Errorf("write segment: %v", err)
		}
	}()
	if !waitForFile(context.Background(), path, time.Second) {
		t.Fatal("expected non-empty segment to become ready")
	}
}
