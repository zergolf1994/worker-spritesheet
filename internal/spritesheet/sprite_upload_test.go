package spritesheet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestUploadSpriteFilesRespectsConcurrencyAndAggregatesProgress(t *testing.T) {
	dir := t.TempDir()
	names := make([]string, 8)
	for index := range names {
		names[index] = fmt.Sprintf("sprite-%d.jpg", index+1)
		if err := os.WriteFile(filepath.Join(dir, names[index]), make([]byte, 10), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var active atomic.Int32
	var maximum atomic.Int32
	var progressRegressed atomic.Bool
	upload := func(_ context.Context, _ string, _ string, _ string, size int64, progress func(int64, int64)) error {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		progress(size/2, size)
		time.Sleep(20 * time.Millisecond)
		progress(size, size)
		return nil
	}

	var lastDone, totalBytes int64
	err := uploadSpriteFiles(context.Background(), dir, "file-id/sprite", names, 4, upload, func(done, total int64) {
		if done < lastDone {
			progressRegressed.Store(true)
		}
		lastDone, totalBytes = done, total
	})
	if err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 4 {
		t.Fatalf("maximum concurrency = %d, want 4", maximum.Load())
	}
	if progressRegressed.Load() {
		t.Fatal("aggregated progress regressed")
	}
	if lastDone != 80 || totalBytes != 80 {
		t.Fatalf("final progress = %d/%d, want 80/80", lastDone, totalBytes)
	}
}
