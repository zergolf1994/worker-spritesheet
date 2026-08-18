package spritesheet

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sync"
)

type spriteUploadFunc func(
	ctx context.Context,
	name, localPath, objectKey string,
	expectedSize int64,
	onProgress func(done, total int64),
) error

type spriteUploadTask struct {
	index        int
	name         string
	localPath    string
	objectKey    string
	expectedSize int64
}

// uploadSpriteFiles uploads separate S3 objects with a bounded worker pool.
// Progress is aggregated across the whole sprite directory and remains
// monotonic even when individual uploads finish out of order.
func uploadSpriteFiles(
	ctx context.Context,
	spriteDir, objectPrefix string,
	names []string,
	concurrency int,
	upload spriteUploadFunc,
	onProgress func(done, total int64),
) error {
	if len(names) == 0 {
		return fmt.Errorf("no sprite files to upload")
	}

	tasks := make([]spriteUploadTask, 0, len(names))
	var totalSize int64
	for index, name := range names {
		localPath := filepath.Join(spriteDir, name)
		info, err := os.Stat(localPath)
		if err != nil {
			return fmt.Errorf("stat %s: %w", name, err)
		}
		tasks = append(tasks, spriteUploadTask{
			index: index, name: name, localPath: localPath,
			objectKey: path.Join(objectPrefix, name), expectedSize: info.Size(),
		})
		totalSize += info.Size()
	}

	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > len(tasks) {
		concurrency = len(tasks)
	}

	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan spriteUploadTask, len(tasks))
	for _, task := range tasks {
		jobs <- task
	}
	close(jobs)

	reported := make([]int64, len(tasks))
	var uploaded int64
	var progressMu sync.Mutex
	updateProgress := func(index int, done int64) {
		progressMu.Lock()
		defer progressMu.Unlock()
		if done < reported[index] {
			return
		}
		if done > tasks[index].expectedSize {
			done = tasks[index].expectedSize
		}
		uploaded += done - reported[index]
		reported[index] = done
		if onProgress != nil {
			onProgress(uploaded, totalSize)
		}
	}

	var firstErr error
	var errOnce sync.Once
	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for task := range jobs {
				if uploadCtx.Err() != nil {
					return
				}
				err := upload(uploadCtx, task.name, task.localPath, task.objectKey, task.expectedSize, func(done, _ int64) {
					updateProgress(task.index, done)
				})
				if err != nil {
					errOnce.Do(func() {
						firstErr = fmt.Errorf("upload %s: %w", task.name, err)
						cancel()
					})
					return
				}
				updateProgress(task.index, task.expectedSize)
			}
		}()
	}
	workers.Wait()

	if firstErr != nil {
		return firstErr
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return nil
}
