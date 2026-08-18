package config

import (
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

// AppConfig holds the application configuration loaded from environment variables.
var AppConfig Config

// Config represents the application configuration.
type Config struct {
	// DashboardPort is opened by worker instance @1 only. All sibling
	// instances publish progress to MongoDB for the shared host dashboard.
	DashboardPort string
	MongoURI      string

	// SpriteGPUEnabled allows NVIDIA NVDEC + scale_cuda. Unsupported inputs or
	// GPU failures automatically retry with the original CPU pipeline.
	SpriteGPUEnabled bool

	StorageId   string
	StoragePath string

	// Number of multipart S3 parts uploaded in parallel.
	S3UploadConcurrency int

	LogPath string // Path to rotating log file (env: LOG_PATH)
}

// Load reads configuration from environment variables (and .env file).
func Load() {
	// Load .env file if present (ignore error if not found)
	godotenv.Load()

	AppConfig = Config{
		DashboardPort:    getEnv("DASHBOARD_PORT", getEnv("PORT", "8887")),
		MongoURI:         getEnv("DATABASE_URL", "mongodb://localhost:27017"),
		SpriteGPUEnabled: getBoolEnv("SPRITESHEET_GPU_ENABLED", true),
		StorageId:        getEnv("STORAGE_ID", ""),
		// ห้ามมี default — ว่างทั้งคู่ = remote mode (main เช็คว่าต้องตั้งคู่กัน)
		StoragePath:         getEnv("STORAGE_PATH", ""),
		S3UploadConcurrency: getIntEnv("S3_UPLOAD_CONCURRENCY", 3, 1, 8),
		LogPath:             getEnv("LOG_PATH", "logs/worker-spritesheet.log"),
	}
}

func getBoolEnv(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getIntEnv(key string, fallback, minValue, maxValue int) int {
	value, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return fallback
	}
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
