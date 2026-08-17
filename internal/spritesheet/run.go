package spritesheet

import (
	"context"
	goerrors "errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"worker-spritesheet/internal/archive"
	"worker-spritesheet/internal/config"
	"worker-spritesheet/internal/core/enums"
	"worker-spritesheet/internal/core/utils"
	"worker-spritesheet/internal/db/models"
	"worker-spritesheet/internal/downloader"
	"worker-spritesheet/internal/queue"
	"worker-spritesheet/internal/uploader"
)

// ─── Spritesheet pipeline ────────────────────────────────────
//
// One job = one file: หาวิดีโอ (resolution เล็กสุด) → ffmpeg สร้าง
// sprite 6x6 + sprite.vtt → ติดตั้ง → สร้าง media thumbnail
//
// 2 โหมดตาม STORAGE_ID/STORAGE_PATH:
//   - co-located (มีทั้งคู่ — รันคู่ storage-node): อ่านวิดีโอตรงจาก
//     {STORAGE_PATH}/{fileId}/ → ผลลัพธ์ย้ายเข้า sprite/ ตรงๆ + สร้าง
//     thumbnail media เอง (ไม่มี network I/O ของวิดีโอเลย)
//   - remote (ไม่มี — เครื่องกลาง): Local media โหลดผ่าน storage-node แล้ว
//     ส่ง sprite.zip ผ่าน Temp/transfer; S3 media โหลดผ่าน originUrl แล้ว
//     อัปโหลด sprite files กลับ permanent S3 + สร้าง thumbnail media โดยตรง
//
// Steps: prepare 15 → generate 70 → install 90 → media 100

// LocalStorageBlockReason is the pre-claim gate for queue.ClaimGate —
// empty when this worker's storage can accept a job right now.
func LocalStorageBlockReason(ctx context.Context) string {
	return localStorageBlockReason(ctx)
}

// Run executes one claimed spritesheet job, then finalizes the per-process log.
func Run(jobCtx context.Context, job *models.VideoProcess) error {
	err := run(jobCtx, job)
	finalizeProcessLog(jobCtx, job, err)
	return err
}

func run(ctx context.Context, job *models.VideoProcess) error {
	fileID := derefStr(job.FileID)
	slug := derefStr(job.Slug)
	if fileID == "" {
		return fmt.Errorf("job has no fileId")
	}

	storagePath := config.AppConfig.StoragePath
	storageID := config.AppConfig.StorageId
	colocated := storageID != "" && storagePath != ""

	// co-located: storage ตัวเองใช้ไม่ได้ชั่วคราว (ปิด/เต็ม) — คืนคิว
	if colocated {
		if reason := localStorageBlockReason(ctx); reason != "" {
			return fmt.Errorf("%s: %w", reason, queue.ErrJobRequeue)
		}
	}

	procLogger := utils.NewProcessLogger(slug)
	defer procLogger.Close()

	exePath, _ := os.Executable()
	baseDir := filepath.Dir(exePath)
	if strings.Contains(exePath, "go-build") {
		baseDir, _ = os.Getwd()
	}
	workDir := filepath.Join(baseDir, "spritesheet", slug)
	os.MkdirAll(workDir, 0755)

	var success bool
	defer func() {
		cancelled := goerrors.Is(context.Cause(ctx), queue.ErrJobCancelled)
		if success || cancelled {
			os.RemoveAll(workDir)
			utils.LogMain("🧹 [%s] Cleaned up temp dir", slug)
		} else {
			utils.LogMain("⚠️  [%s] Keeping temp dir for retry: %s", slug, workDir)
		}
	}()

	utils.LogMain("🖼️  [%s] START SPRITESHEET (storage=%s)", slug, storageID)

	file, err := models.FileModel.FindByID(ctx, fileID)
	if err != nil {
		return fmt.Errorf("file not found: %w", err)
	}

	// มี thumbnail แล้ว (ทำไปแล้ว/transfer ติดตั้ง sprite.zip ไปแล้ว) — จบงานเฉยๆ
	if hasThumbnailMedia(ctx, fileID) {
		utils.LogMain("⏭️  [%s] thumbnail media already exists — completing", slug)
		success = true
		return nil
	}
	// sprite.zip ค้างรอ worker-transfer ติดตั้งอยู่ — ไม่ทำซ้ำ
	if hasPendingSpriteIngest(ctx, fileID) {
		utils.LogMain("⏭️  [%s] sprite.zip ingest pending (await worker-transfer) — completing", slug)
		success = true
		return nil
	}

	// ─── STEP 1: PREPARE — หา input ───────────────────────────
	startStep(ctx, job.ID, "prepare")

	videoMedia, err := findSmallestVideo(ctx, fileID)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}

	var inputPath string
	var sourceStorage *models.Storage
	if colocated {
		if derefStr(videoMedia.StorageID) != storageID {
			// enqueuer จ่ายงานตาม storage ของ media — ถ้าไม่ตรงแปลว่า media
			// ย้ายไปแล้วหลังเข้าคิว ให้คืนคิว (enqueuer รอบใหม่จะจ่ายให้ถูกเครื่อง)
			return fmt.Errorf("video media moved to storage %s: %w",
				derefStr(videoMedia.StorageID), queue.ErrJobRequeue)
		}
		videoFileName := derefStr(videoMedia.FileName)
		inputPath = filepath.Join(storagePath, fileID, videoFileName)
		if _, statErr := os.Stat(inputPath); statErr != nil {
			return fmt.Errorf("prepare: local video missing: %s", inputPath)
		}
	} else {
		// remote: โหลดผ่าน storage-node HTTP ของเครื่องที่ media อยู่
		var sourceErr error
		sourceStorage, sourceErr = models.StorageModel.FindByID(ctx, derefStr(videoMedia.StorageID))
		if sourceErr != nil {
			return fmt.Errorf("prepare: source storage not found: %w", sourceErr)
		}
		if !sourceStorage.IsOnline() {
			return fmt.Errorf("prepare: source storage unavailable: %w", queue.ErrJobRequeue)
		}
		var sourceURL string
		if sourceStorage.Type == enums.StorageTypeS3 {
			sourceURL, sourceErr = sourceStorage.GetOriginObjectURL(fileID, derefStr(videoMedia.FileName))
			if sourceErr != nil {
				return fmt.Errorf("prepare: resolve S3 origin: %w", sourceErr)
			}
		} else {
			hostPort := sourceStorage.GetHostPort()
			if hostPort == "" {
				return fmt.Errorf("prepare: source storage has no host")
			}
			sourceURL = fmt.Sprintf("http://%s/%s.mp4", hostPort, videoMedia.Slug)
		}
		inputPath = filepath.Join(workDir, "input.mp4")
		utils.LogMain("📥 [%s] Downloading %s", slug, sourceURL)
		if dErr := downloader.DownloadURL(ctx, sourceURL, inputPath, pctLogger64(slug, "prepare")); dErr != nil {
			return fmt.Errorf("prepare: download: %w", dErr)
		}
	}

	duration := fileDuration(file)
	if duration <= 0 {
		if info, probeErr := ProbeVideoInfo(inputPath); probeErr == nil {
			duration = info.DurationF
		}
	}

	completeStep(ctx, job.ID, "prepare")
	utils.LogMain("📂 [%s] Input: %s (%.1fs)", slug, inputPath, duration)

	// ─── STEP 2: GENERATE sprite sheets (ffmpeg) ──────────────
	startStep(ctx, job.ID, "generate")

	result, err := Generate(inputPath, workDir, duration)
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}
	os.Remove(filepath.Join(result.SpriteDir, "cropped_last.jpg"))

	completeStep(ctx, job.ID, "generate")
	utils.LogMain("✅ [%s] Generated %d sprite file(s)", slug, len(result.SpriteFiles))

	var totalSpriteSize int64
	for _, name := range result.SpriteFiles {
		totalSpriteSize += GetFileSize(filepath.Join(result.SpriteDir, name))
	}

	if colocated {
		// ─── STEP 3: INSTALL → {storagePath}/{fileId}/sprite/ ─
		startStep(ctx, job.ID, "install")

		if err := installDir(storagePath, fileID, "sprite", result.SpriteDir); err != nil {
			return fmt.Errorf("install sprite: %w", err)
		}
		completeStep(ctx, job.ID, "install")
		utils.LogMain("📂 [%s] Installed sprite/ → %s/%s/sprite/", slug, storagePath, fileID)

		// ─── STEP 4: MEDIA RECORD (thumbnail) ─────────────────
		startStep(ctx, job.ID, "media")

		if !hasThumbnailMedia(ctx, fileID) {
			now := time.Now()
			thumbFn := enums.SpriteVTTName
			sid := storageID
			thumbMedia := models.Media{
				ID: newUUID(), Type: enums.MediaTypeThumbnail, FileName: &thumbFn,
				StorageID: &sid, Slug: utils.RandomString(11, false), FileID: &fileID,
				Metadata:  &models.MediaMetadata{Size: totalSpriteSize, Duration: duration},
				CreatedAt: now, UpdatedAt: now,
			}
			if _, err := models.MediaModel.Create(ctx, &thumbMedia); err != nil {
				return fmt.Errorf("create media thumbnail: %w", err)
			}
			cloneMediaToClonedFiles(ctx, fileID, thumbMedia, slug)
			utils.LogMain("✅ [%s] Media record: thumbnail", slug)
		}
		completeStep(ctx, job.ID, "media")
	} else if sourceStorage != nil && sourceStorage.Type == enums.StorageTypeS3 {
		// ─── STEP 3 (S3 source): upload final sprite objects directly ─
		startStep(ctx, job.ID, "install")
		for _, name := range result.SpriteFiles {
			localPath := filepath.Join(result.SpriteDir, name)
			size := GetFileSize(localPath)
			objectKey := path.Join(fileID, "sprite", name)
			if err := uploader.VerifyS3Object(ctx, sourceStorage, objectKey, size); err != nil {
				utils.LogMain("📤 [%s] Uploading %s → permanent S3...", slug, objectKey)
				if err := uploader.UploadToS3(ctx, sourceStorage, localPath, objectKey, nil); err != nil {
					return fmt.Errorf("upload %s: %w", name, err)
				}
				if err := uploader.VerifyS3Object(ctx, sourceStorage, objectKey, size); err != nil {
					return fmt.Errorf("verify %s: %w", name, err)
				}
			}
		}
		completeStep(ctx, job.ID, "install")

		// สร้าง media หลังทุก object ตรวจสอบผ่านแล้วเท่านั้น
		startStep(ctx, job.ID, "media")
		if !hasThumbnailMedia(ctx, fileID) {
			now := time.Now()
			thumbFn := enums.SpriteVTTName
			sid := sourceStorage.ID
			thumbMedia := models.Media{
				ID: newUUID(), Type: enums.MediaTypeThumbnail, FileName: &thumbFn,
				StorageID: &sid, Slug: utils.RandomString(11, false), FileID: &fileID,
				Metadata:  &models.MediaMetadata{Size: totalSpriteSize, Duration: duration},
				CreatedAt: now, UpdatedAt: now,
			}
			if _, err := models.MediaModel.Create(ctx, &thumbMedia); err != nil {
				return fmt.Errorf("create S3 thumbnail media: %w", err)
			}
			cloneMediaToClonedFiles(ctx, fileID, thumbMedia, slug)
			utils.LogMain("✅ [%s] S3 thumbnail media created", slug)
		}
		completeStep(ctx, job.ID, "media")
	} else {
		// ─── STEP 3 (remote): zip → S3 temp ───────────────────
		startStep(ctx, job.ID, "install")

		zipPath := filepath.Join(workDir, enums.SpriteZipName)
		if err := archive.ZipDir(result.SpriteDir, zipPath); err != nil {
			return fmt.Errorf("zip sprite: %w", err)
		}
		s3Storage, err := resolveS3TempStorage(ctx)
		if err != nil {
			// ไม่มี S3 temp = ปัญหาสภาพแวดล้อม ไม่ใช่ความผิดของงาน — คืนคิว
			return fmt.Errorf("%v: %w", err, queue.ErrJobRequeue)
		}
		// key แบบมีวันที่ เหมือน worker-download ({date}/{fileId}_{fileName})
		// — worker-transfer อ่านจาก ingest.path อยู่แล้ว ไม่ประกอบ key เอง
		objectKey := fmt.Sprintf("%s/%s_%s", time.Now().Format("2006-01-02"), fileID, enums.SpriteZipName)
		utils.LogMain("📤 [%s] Uploading %s → S3 temp...", slug, objectKey)
		if err := uploader.UploadToS3(ctx, s3Storage, zipPath, objectKey, pctLogger64(slug, "install")); err != nil {
			return fmt.Errorf("upload sprite.zip: %w", err)
		}
		completeStep(ctx, job.ID, "install")

		// ─── STEP 4 (remote): ingest ให้ worker-transfer ──────
		startStep(ctx, job.ID, "media")
		zipSize := GetFileSize(zipPath)
		if err := createSpriteIngest(ctx, fileID, s3Storage, objectKey, zipSize); err != nil {
			return fmt.Errorf("create ingest: %w", err)
		}
		completeStep(ctx, job.ID, "media")
		utils.LogMain("✅ [%s] Uploaded %s + ingest (worker-transfer will install)", slug, objectKey)
	}

	success = true
	utils.LogMain("✅ [%s] SPRITESHEET COMPLETE (%d sheet(s), %.1fs video)", slug, len(result.SpriteFiles)-1, duration)
	return nil
}

func fileDuration(file *models.File) float64 {
	if file.Metadata != nil && file.Metadata.Duration != nil {
		return *file.Metadata.Duration
	}
	return 0
}
