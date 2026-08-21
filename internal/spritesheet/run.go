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
	storageBound := storageID != "" && storagePath != ""
	configuredLocal := false
	if storageBound {
		configuredStorage, storageErr := models.StorageModel.FindByID(ctx, storageID)
		if storageErr != nil {
			return fmt.Errorf("configured storage not found: %w", storageErr)
		}
		configuredLocal = configuredStorage.Type == enums.StorageTypeLocal
	}

	// co-located: storage ตัวเองใช้ไม่ได้ชั่วคราว (ปิด/เต็ม) — คืนคิว
	if configuredLocal {
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

	sourceStorage, sourceErr := models.StorageModel.FindByID(ctx, derefStr(videoMedia.StorageID))
	if sourceErr != nil {
		return fmt.Errorf("prepare: source storage not found: %w", sourceErr)
	}
	// enable=false means the storage no longer accepts new files. Existing
	// objects remain readable and may still be used as a spritesheet source.
	if !sourceStorageReadable(sourceStorage) {
		return fmt.Errorf("prepare: source storage unavailable: %w", queue.ErrJobRequeue)
	}
	useLocalDisk := shouldUseLocalDisk(storageID, storagePath, sourceStorage)

	var inputPath string
	if useLocalDisk {
		objectPath := videoMedia.ObjectPath()
		if objectPath == "" {
			return fmt.Errorf("prepare: media object path is empty")
		}
		inputPath = filepath.Join(storagePath, filepath.FromSlash(objectPath))
		if _, statErr := os.Stat(inputPath); statErr != nil {
			return fmt.Errorf("prepare: local video missing: %s", inputPath)
		}
	} else {
		// remote: Local โหลดผ่าน storage-node; S3 โหลด raw object ผ่าน originUrl
		var sourceURL string
		if sourceStorage.Type == enums.StorageTypeS3 {
			sourceURL, sourceErr = sourceStorage.GetOriginObjectPathURL(videoMedia.ObjectPath())
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
		writePrepare := stepThrottle(1)
		logPrepare := pctLogger64(slug, "prepare")
		if dErr := downloader.DownloadURL(ctx, sourceURL, inputPath, func(done, total int64) {
			logPrepare(done, total)
			if total > 0 {
				pct := float64(done) / float64(total) * 100
				if writePrepare(pct) {
					updateStepAndOverall(ctx, job.ID, "prepare", pct, pct*0.15)
				}
			}
		}); dErr != nil {
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

	writeGenerate := stepThrottle(1)
	logGenerate := pctLogger(slug, "generate")
	result, err := Generate(ctx, inputPath, workDir, duration, config.AppConfig.SpriteGPUEnabled, func(percent int) {
		logGenerate(percent)
		pct := float64(percent)
		if writeGenerate(pct) {
			updateStepAndOverall(ctx, job.ID, "generate", pct, 15+pct*0.55)
		}
	})
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

	if useLocalDisk {
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
		writeInstall := stepThrottle(1)
		logInstall := pctLogger64(slug, "install")
		uploadOne := func(uploadCtx context.Context, name, localPath, objectKey string, size int64, onProgress func(int64, int64)) error {
			if err := uploader.VerifyS3Object(uploadCtx, sourceStorage, objectKey, size); err == nil {
				onProgress(size, size)
				return nil
			}
			utils.LogMain("📤 [%s] Uploading %s → permanent S3...", slug, objectKey)
			if err := uploader.UploadToS3(uploadCtx, sourceStorage, localPath, objectKey, onProgress); err != nil {
				return err
			}
			return uploader.VerifyS3Object(uploadCtx, sourceStorage, objectKey, size)
		}
		utils.LogMain("📤 [%s] Uploading %d sprite object(s), concurrency=%d", slug, len(result.SpriteFiles), config.AppConfig.SpriteUploadConcurrency)
		if err := uploadSpriteFiles(ctx, result.SpriteDir, path.Join(fileID, "sprite"), result.SpriteFiles,
			config.AppConfig.SpriteUploadConcurrency, uploadOne, func(done, total int64) {
				logInstall(done, total)
				if total <= 0 {
					return
				}
				pct := float64(done) / float64(total) * 100
				if writeInstall(pct) {
					updateStepAndOverall(ctx, job.ID, "install", pct, 70+pct*0.20)
				}
			}); err != nil {
			return fmt.Errorf("upload sprite directory: %w", err)
		}
		completeStep(ctx, job.ID, "install")

		// สร้าง media หลังทุก object ตรวจสอบผ่านแล้วเท่านั้น
		startStep(ctx, job.ID, "media")
		if !hasThumbnailMedia(ctx, fileID) {
			now := time.Now()
			thumbFn := enums.SpriteVTTName
			sid := sourceStorage.ID
			thumbPath := path.Join(fileID, "sprite")
			thumbMedia := models.Media{
				ID: newUUID(), Type: enums.MediaTypeThumbnail, FileName: &thumbFn,
				StorageID: &sid, Slug: utils.RandomString(11, false), Path: &thumbPath, FileID: &fileID,
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
		writeInstall := stepThrottle(1)
		logInstall := pctLogger64(slug, "install")
		if err := uploader.UploadToS3(ctx, s3Storage, zipPath, objectKey, func(done, total int64) {
			logInstall(done, total)
			if total > 0 {
				pct := float64(done) / float64(total) * 100
				if writeInstall(pct) {
					updateStepAndOverall(ctx, job.ID, "install", pct, 70+pct*0.20)
				}
			}
		}); err != nil {
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

func shouldUseLocalDisk(configuredStorageID, storagePath string, sourceStorage *models.Storage) bool {
	return sourceStorage != nil &&
		sourceStorage.Type == enums.StorageTypeLocal &&
		configuredStorageID != "" && storagePath != "" &&
		sourceStorage.ID == configuredStorageID
}

func sourceStorageReadable(storage *models.Storage) bool {
	return storage != nil && storage.Status == enums.StorageStatusOnline
}
