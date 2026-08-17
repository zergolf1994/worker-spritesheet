package spritesheet

import (
	"testing"

	"worker-spritesheet/internal/db/models"
)

func TestShouldUseLocalDiskRejectsS3Storage(t *testing.T) {
	storage := &models.Storage{ID: "storage-1", Type: "s3"}
	if shouldUseLocalDisk("storage-1", "/home/files", storage) {
		t.Fatal("S3 storage must use originUrl even when the worker is storage-bound")
	}
}

func TestShouldUseLocalDiskAcceptsMatchingLocalStorage(t *testing.T) {
	storage := &models.Storage{ID: "storage-1", Type: "local"}
	if !shouldUseLocalDisk("storage-1", "/home/files", storage) {
		t.Fatal("matching local storage should use the local disk")
	}
}
