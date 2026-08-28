package transcode

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestSessionBufferTicksIgnoresRequestedButUnreadySegments(t *testing.T) {
	dir := t.TempDir()
	session := &Session{
		Dir:                dir,
		HighestSegmentSeen: 4,
		SegmentTicks:       defaultSegmentTicks,
	}

	generated, _, buffered := sessionBufferTicks(session)
	if generated != 0 || buffered != 0 {
		t.Fatalf("unready buffer generated=%d buffered=%d", generated, buffered)
	}

	writeTestSegmentFiles(t, dir, 0, 1)
	generated, _, buffered = sessionBufferTicks(session)
	if generated != 4*timeSecondTicks || buffered != 4*timeSecondTicks {
		t.Fatalf("ready buffer generated=%d buffered=%d count=%d", generated, buffered, session.ReadySegmentCount)
	}
	if session.HighestSegmentSeen != 4 {
		t.Fatalf("HighestSegmentSeen = %d", session.HighestSegmentSeen)
	}
}

func TestSessionBufferTicksIncludesUnrequestedReadySegments(t *testing.T) {
	dir := t.TempDir()
	session := &Session{
		Dir:                dir,
		StartTimeTicks:     1571 * defaultSegmentTicks,
		SegmentStartIndex:  1571,
		HighestSegmentSeen: 1571,
		SegmentTicks:       defaultSegmentTicks,
	}

	generated, played, buffered := sessionBufferTicks(session)
	if generated != session.StartTimeTicks || played != session.StartTimeTicks || buffered != 0 {
		t.Fatalf("seeked unready generated=%d played=%d buffered=%d", generated, played, buffered)
	}

	writeTestSegmentFiles(t, dir, 1571, 1573)
	generated, _, buffered = sessionBufferTicks(session)
	if session.ReadySegmentCount != 3 {
		t.Fatalf("ReadySegmentCount = %d", session.ReadySegmentCount)
	}
	if generated != session.StartTimeTicks+6*timeSecondTicks || buffered != 6*timeSecondTicks {
		t.Fatalf("seeked ready generated=%d buffered=%d", generated, buffered)
	}
}

func TestSessionBufferTicksStopsAtMissingSegment(t *testing.T) {
	dir := t.TempDir()
	session := &Session{
		Dir:          dir,
		SegmentTicks: defaultSegmentTicks,
	}
	writeTestSegmentFiles(t, dir, 0, 1)
	writeTestSegmentFiles(t, dir, 3, 4)

	generated, _, buffered := sessionBufferTicks(session)
	if session.ReadySegmentCount != 2 || generated != 4*timeSecondTicks || buffered != 4*timeSecondTicks {
		t.Fatalf("hole buffer count=%d generated=%d buffered=%d", session.ReadySegmentCount, generated, buffered)
	}
}

func writeTestSegmentFiles(t *testing.T, dir string, from, to int) {
	t.Helper()
	for index := from; index <= to; index++ {
		path := filepath.Join(dir, fmt.Sprintf("segment_%05d.ts", index))
		if err := os.WriteFile(path, []byte("ts"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
