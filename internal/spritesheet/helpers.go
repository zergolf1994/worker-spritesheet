package spritesheet

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"worker-spritesheet/internal/config"
	"worker-spritesheet/internal/core/enums"
	"worker-spritesheet/internal/core/utils"
	"worker-spritesheet/internal/db/models"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func newUUID() string { return uuid.New().String() }

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ─── Local storage gating ─────────────────────────────────────
// A spritesheet worker runs on the same machine as one local storage
// (STORAGE_ID) and reads video files straight from STORAGE_PATH. The
// enqueuer assigns jobs by the storage the video media lives on
// (targetStorageId) — this worker only checks its own storage is usable.

const storageCapacityMaxPercent = 90.0

// localStorageBlockReason returns why this worker's STORAGE_ID cannot
// accept jobs right now (empty = ok).
func localStorageBlockReason(ctx context.Context) string {
	storageID := config.AppConfig.StorageId
	if storageID == "" {
		return "storage_not_configured"
	}
	storage, err := models.StorageModel.FindByID(ctx, storageID)
	if err != nil {
		return "local_storage_not_found"
	}
	if !storage.Enable {
		return "local_storage_disabled"
	}
	if storage.Status != enums.StorageStatusOnline {
		return "local_storage_not_online"
	}
	if storage.Capacity != nil && storage.Capacity.Percentage >= storageCapacityMaxPercent {
		return "local_storage_capacity_full"
	}
	return ""
}

// ─── Media lookups ───────────────────────────────────────────

// hasThumbnailMedia checks medias collection globally (any storage).
func hasThumbnailMedia(ctx context.Context, fileID string) bool {
	count, _ := models.MediaModel.CountDocuments(ctx, bson.M{
		"fileId":    fileID,
		"type":      enums.MediaTypeThumbnail,
		"deletedAt": bson.M{"$exists": false},
	})
	return count > 0
}

// findSmallestVideo returns the smallest-resolution video media of the
// file (360 → 480 → 720 → 1080 → original) — sprite frames are tiny, no
// point decoding the biggest rendition.
func findSmallestVideo(ctx context.Context, fileID string) (*models.Media, error) {
	for _, res := range []string{
		enums.Resolution360, enums.Resolution480,
		enums.Resolution720, enums.Resolution1080,
		enums.ResolutionOriginal,
	} {
		media, err := models.MediaModel.FindOne(ctx, bson.M{
			"fileId":     fileID,
			"type":       enums.MediaTypeVideo,
			"resolution": res,
			"deletedAt":  bson.M{"$exists": false},
		})
		if err == nil {
			return media, nil
		}
	}
	return nil, fmt.Errorf("no video media for file %s", fileID)
}

// ─── Sprite ingest (โหมด remote) ─────────────────────────────
// remote worker อัพ sprite.zip ขึ้น S3 + สร้าง ingest processed —
// worker-transfer บนเครื่อง storage เป็นคนติดตั้ง + สร้าง thumbnail media

// hasPendingSpriteIngest — sprite.zip อัพไปแล้ว รอ worker-transfer ติดตั้ง
func hasPendingSpriteIngest(ctx context.Context, fileID string) bool {
	count, _ := models.IngestModel.CountDocuments(ctx, bson.M{
		"fileId":     fileID,
		"fileName":   enums.SpriteZipName,
		"sourceType": enums.IngestSourceTypeProcessed,
		"deletedAt":  bson.M{"$exists": false},
	})
	return count > 0
}

func createSpriteIngest(ctx context.Context, fileID string, s3Storage *models.Storage, objectKey string, zipSize int64) error {
	if hasPendingSpriteIngest(ctx, fileID) {
		return nil
	}
	now := time.Now()
	storageID := s3Storage.ID
	key := objectKey
	ingest := models.Ingest{
		ID:         newUUID(),
		FileID:     &fileID,
		StorageID:  &storageID,
		FileName:   enums.SpriteZipName,
		Status:     "completed",
		Size:       zipSize,
		Path:       &key,
		SourceType: enums.IngestSourceTypeProcessed,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if _, err := models.IngestModel.Create(ctx, &ingest); err != nil {
		return err
	}
	log.Printf("✅ Created ingest: fileId=%s fileName=%s path=%s", fileID, enums.SpriteZipName, key)
	return nil
}

// ─── Clone propagation ───────────────────────────────────────

func cloneMediaToClonedFiles(ctx context.Context, sourceFileID string, media models.Media, slug string) {
	cursor, err := models.FileModel.FindRaw(ctx, bson.M{
		"clonedFrom":         sourceFileID,
		"type":               enums.FileTypeVideo,
		"metadata.trashedAt": bson.M{"$exists": false},
		"metadata.deletedAt": bson.M{"$exists": false},
	})
	if err != nil {
		return
	}
	defer cursor.Close(ctx)

	for cursor.Next(ctx) {
		var clonedFile models.File
		if err := cursor.Decode(&clonedFile); err != nil {
			continue
		}

		filter := bson.M{"fileId": clonedFile.ID, "type": media.Type}
		if media.Resolution != nil {
			filter["resolution"] = *media.Resolution
		}
		existCount, _ := models.MediaModel.CountDocuments(ctx, filter)
		if existCount > 0 {
			continue
		}

		now := time.Now()
		clonedMedia := models.Media{
			ID:         newUUID(),
			Type:       media.Type,
			FileName:   media.FileName,
			MimeType:   media.MimeType,
			Resolution: media.Resolution,
			StorageID:  media.StorageID,
			Slug:       utils.RandomString(11, true),
			Path:       media.Path,
			FileID:     &clonedFile.ID,
			Metadata:   media.Metadata,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		clonedFrom := sourceFileID
		if media.ClonedFrom != nil && strings.TrimSpace(*media.ClonedFrom) != "" {
			clonedFrom = strings.TrimSpace(*media.ClonedFrom)
		}
		clonedMedia.ClonedFrom = &clonedFrom

		if _, err := models.MediaModel.Create(ctx, &clonedMedia); err != nil {
			log.Printf("⚠️  [%s] Failed to clone media to %s: %v", slug, clonedFile.ID, err)
			continue
		}
		log.Printf("📋 [%s] Cloned media → file %s", slug, clonedFile.ID)
	}
}

// ─── S3 temp storage (log upload) ────────────────────────────

// resolveS3TempStorage finds an S3 storage that accepts ["temp", "video"].
func resolveS3TempStorage(ctx context.Context) (*models.Storage, error) {
	filter := bson.M{
		"enable":  true,
		"status":  enums.StorageStatusOnline,
		"type":    enums.StorageTypeS3,
		"accepts": bson.M{"$all": []string{enums.StorageAcceptTemp, enums.StorageAcceptVideo}},
	}
	storage, err := models.StorageModel.FindOne(ctx, filter, options.FindOne().SetSort(bson.M{"capacity.percentage": 1}))
	if err != nil {
		return nil, fmt.Errorf("no S3 temp storage available")
	}
	return storage, nil
}
