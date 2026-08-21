package spritesheet

import (
	"testing"

	"worker-spritesheet/internal/core/enums"
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

func TestSourceStorageReadableAllowsDisabledOnlineStorage(t *testing.T) {
	storage := &models.Storage{
		Enable: false,
		Status: enums.StorageStatusOnline,
	}
	if !sourceStorageReadable(storage) {
		t.Fatal("disabled online storage must remain readable as a source")
	}
}

func TestSourceStorageReadableRejectsOfflineStorage(t *testing.T) {
	storage := &models.Storage{
		Enable: true,
		Status: enums.StorageStatusOffline,
	}
	if sourceStorageReadable(storage) {
		t.Fatal("offline storage must not be used as a source")
	}
}
