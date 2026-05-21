package transcode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveFailedTranscodeLogCopiesLogOutsideSessionDir(t *testing.T) {
	tempDir := t.TempDir()
	session := &Session{
		ID:  "item123",
		Dir: filepath.Join(tempDir, "item123"),
	}
	if err := os.MkdirAll(session.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(session.Dir, "ffmpeg.log")
	want := "ffmpeg failed\nstream mapping\n"
	if err := os.WriteFile(logPath, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}

	archivedPath, err := archiveFailedTranscodeLog(session, logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(archivedPath, filepath.Join(tempDir, "logs")) {
		t.Fatalf("archived path = %q", archivedPath)
	}
	got, err := os.ReadFile(archivedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("archived contents = %q", got)
	}
}
