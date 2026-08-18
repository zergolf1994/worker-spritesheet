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

// ─── Realtime progress writes ────────────────────────────────
// Long-running I/O and FFmpeg work write at most once per 1%, allowing the
// dashboard to update in realtime without an unbounded number of DB writes.

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

func updateStepAndOverall(ctx context.Context, processID, step string, stepPercent, overallPercent float64) {
	if stepPercent < 0 {
		stepPercent = 0
	}
	if stepPercent > 100 {
		stepPercent = 100
	}
	if overallPercent > 100 {
		overallPercent = 100
	}
	models.VideoProcessModel.UpdateByID(ctx, processID, bson.M{"$set": bson.M{
		fmt.Sprintf("timeline.%s.status", step):  enums.StepStatusProcessing,
		fmt.Sprintf("timeline.%s.percent", step): stepPercent,
		"overallPercent":                         overallPercent,
	}})
}

func stepThrottle(stepPct float64) func(float64) bool {
	last := -stepPct
	return func(pct float64) bool {
		if pct-last >= stepPct || pct >= 100 {
			last = pct
			return true
		}
		return false
	}
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

// pctLogger returns a callback that logs each integer milestone once.
func pctLogger(slug, step string) func(int) {
	nextMilestone := 0
	return func(percent int) {
		if percent > 100 {
			percent = 100
		}
		for nextMilestone <= percent && nextMilestone <= 100 {
			log.Printf("📊 [%s] %s: %d%%", slug, step, nextMilestone)
			nextMilestone++
		}
	}
}
