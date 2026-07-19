package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"worker-spritesheet/internal/config"
	"worker-spritesheet/internal/core/logger"
	"worker-spritesheet/internal/core/utils"
	"worker-spritesheet/internal/db/database"
	"worker-spritesheet/internal/queue"
	"worker-spritesheet/internal/spritesheet"
)

// version ถูกฝังตอน build โดย GitHub Actions: -ldflags="-X main.version=v1.x.x"
var version = "dev"

func main() {
	config.Load()
	workerID := utils.GenerateWorkerID()
	log.Printf("🚀 Starting Worker Spritesheet %s [Worker: %s]", version, workerID)

	// 2 โหมด:
	//   - co-located: มี STORAGE_ID + STORAGE_PATH → อ่าน/เขียนดิสก์ตรง
	//     (Claim กรอง targetStorageId = STORAGE_ID)
	//   - remote pool: ไม่มีทั้งคู่ → โหลดวิดีโอผ่าน HTTP, ผลลัพธ์อัพ S3
	//     ให้ worker-transfer ติดตั้ง (Claim เฉพาะงานไม่ผูก storage)
	colocated := config.AppConfig.StorageId != "" && config.AppConfig.StoragePath != ""
	if !colocated && (config.AppConfig.StorageId != "" || config.AppConfig.StoragePath != "") {
		log.Println("❌ STORAGE_ID and STORAGE_PATH must be set together (or both empty for remote mode)")
		time.Sleep(5 * time.Second) // กัน systemd restart-loop รัวๆ
		os.Exit(1)
	}
	if colocated {
		log.Printf("📍 Mode: co-located (storage=%s path=%s)", config.AppConfig.StorageId, config.AppConfig.StoragePath)
	} else {
		log.Println("📍 Mode: remote pool (download via storage-node HTTP, results → S3 + ingest)")
	}

	// ffmpeg/ffprobe คือหัวใจของงานนี้ — ไม่มีก็ทำอะไรไม่ได้เลย
	if err := spritesheet.CheckFFmpeg(); err != nil {
		log.Printf("❌ %v", err)
		time.Sleep(5 * time.Second)
		os.Exit(1)
	}

	// ── Rotating file logger ──────────────────────────────────
	logCloser, err := logger.Init(config.AppConfig.LogPath)
	if err != nil {
		log.Printf("⚠️ File logging disabled: %v", err)
	} else {
		defer logCloser.Close()
		log.Printf("📝 Logging to: %s", config.AppConfig.LogPath)
	}

	// ── MongoDB ───────────────────────────────────────────────
	if err := database.Connect(); err != nil {
		log.Printf("❌ Failed to connect to MongoDB: %v", err)
		time.Sleep(5 * time.Second) // ให้ log ถูก flush / กัน restart-loop รัวๆ
		os.Exit(1)
	}
	defer database.Disconnect()

	// ── Heartbeat ─────────────────────────────────────────────
	// ctx ยกเลิกเมื่อโดน SIGINT/SIGTERM → heartbeat mark ตัวเอง offline ก่อนจบ
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		queue.StartHeartbeat(ctx, workerID)
	}()

	// ── Job loop (blocking จนโดน SIGINT/SIGTERM) ──────────────
	// shutdown ระหว่างทำงาน → loop จะ Release งานคืนคิวให้เอง
	// gate (เฉพาะ co-located): storage ตัวเองปิด/เต็ม/ออฟไลน์ → หยุดหยิบงาน
	if colocated {
		queue.ClaimGate = spritesheet.LocalStorageBlockReason
	}
	queue.RunLoop(ctx, workerID, spritesheet.Run)

	log.Println("🛑 Shutting down...")

	// รอ heartbeat ปิดตัว (mark offline) ให้เสร็จก่อน disconnect DB
	select {
	case <-hbDone:
	case <-time.After(10 * time.Second):
		log.Println("⚠️ Heartbeat shutdown timed out")
	}
}
