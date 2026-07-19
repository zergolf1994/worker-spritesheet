package spritesheet

import (
	"context"
	"fmt"
	"log"
	"time"

	"worker-spritesheet/internal/core/enums"
	"worker-spritesheet/internal/db/models"

	"go.mongodb.org/mongo-driver/bson"
)

// ─── Step-only DB writes ─────────────────────────────────────
// No realtime % to the DB — progress goes to the per-process log only
// (throttled). The DB is written at step boundaries: overallPercent
// 15/70/90/100 for prepare/generate/install/media.

var stepPercent = map[string]float64{
	"prepare":  15,
	"generate": 70,
	"install":  90,
	"media":    100,
}

func startStep(ctx context.Context, processID, step string) {
	now := time.Now()
	models.VideoProcessModel.UpdateByID(ctx, processID, bson.M{"$set": bson.M{
		fmt.Sprintf("timeline.%s.status", step):    enums.StepStatusProcessing,
		fmt.Sprintf("timeline.%s.percent", step):   0,
		fmt.Sprintf("timeline.%s.startedAt", step): now,
	}})
}

func completeStep(ctx context.Context, processID, step string) {
	now := time.Now()
	set := bson.M{
		fmt.Sprintf("timeline.%s.status", step):  enums.StepStatusCompleted,
		fmt.Sprintf("timeline.%s.percent", step): 100,
		fmt.Sprintf("timeline.%s.endedAt", step): now,
	}
	if pct, ok := stepPercent[step]; ok {
		set["overallPercent"] = pct
	}
	models.VideoProcessModel.UpdateByID(ctx, processID, bson.M{"$set": set})
}

// pctLogger64 returns a bytes-progress callback that logs every ~10%.
func pctLogger64(slug, step string) func(done, total int64) {
	lastPct := -10.0
	return func(done, total int64) {
		if total <= 0 {
			return
		}
		pct := float64(done) / float64(total) * 100
		if pct-lastPct >= 10 || pct >= 100 {
			log.Printf("📊 [%s] %s: %.1f%% (%.2f / %.2f MB)", slug, step, pct,
				float64(done)/1024/1024, float64(total)/1024/1024)
			lastPct = pct
		}
	}
}
