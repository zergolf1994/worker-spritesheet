package spritesheet

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	spriteCols     = 6
	spriteMaxRows  = 6
	spriteInterval = 1.0 // 1000ms per frame
)

// Result contains generated sprite output.
type Result struct {
	SpriteDir   string
	SpriteFiles []string
	VTTFile     string
}

// Generate creates sprite sheets (6x6, 1s interval) under {outputDir}/sprite/.
func Generate(ctx context.Context, inputPath, outputDir string, duration float64, gpuEnabled bool, onProgress func(int)) (*Result, error) {
	if duration <= 0 {
		info, err := ProbeVideoInfo(inputPath)
		if err != nil || info.DurationF <= 0 {
			return nil, fmt.Errorf("invalid duration: %.2f", duration)
		}
		duration = info.DurationF
	}

	spriteDir := filepath.Join(outputDir, "sprite")
	os.MkdirAll(spriteDir, 0755)

	framesPerSheet := spriteCols * spriteMaxRows
	totalFrames := int(math.Floor(duration / spriteInterval))
	if totalFrames < 1 {
		totalFrames = 1
	}

	thumbWidth, thumbHeight := calcThumbSize(inputPath)
	log.Printf("🧠 Generating %d thumbnails (interval = %.0fms, size = %dx%d)", totalFrames, spriteInterval*1000, thumbWidth, thumbHeight)

	fpsFilter := fmt.Sprintf("fps=1/%.2f", spriteInterval)
	scaleFilter := fmt.Sprintf("scale=%d:%d", thumbWidth, thumbHeight)
	tileFilter := fmt.Sprintf("tile=%dx%d", spriteCols, spriteMaxRows)
	spritePattern := filepath.Join(spriteDir, "sprite-%d.jpg")
	totalSheets := int(math.Ceil(float64(totalFrames) / float64(framesPerSheet)))

	usedGPU := false
	if available, reason := nvidiaSpriteAvailable(gpuEnabled); available {
		utilsLogGPU(inputPath)
		gpuFilter := fmt.Sprintf("%s,scale_cuda=%d:%d,hwdownload,format=nv12,%s", fpsFilter, thumbWidth, thumbHeight, tileFilter)
		gpuArgs := spriteFFmpegArgs(inputPath, spritePattern, gpuFilter, true)
		if err := runSpriteFFmpeg(ctx, gpuArgs, totalSheets, onProgress); err == nil {
			usedGPU = true
		} else {
			if cause := context.Cause(ctx); cause != nil {
				return nil, cause
			}
			log.Printf("⚠️  GPU sprite generation failed: %v — retrying with CPU", err)
			removeSpriteOutputs(spritePattern)
		}
	} else if gpuEnabled {
		log.Printf("💻 NVIDIA sprite acceleration unavailable (%s) — using CPU", reason)
	}

	if !usedGPU {
		cpuFilter := fmt.Sprintf("%s,%s,%s", fpsFilter, scaleFilter, tileFilter)
		cpuArgs := spriteFFmpegArgs(inputPath, spritePattern, cpuFilter, false)
		if err := runSpriteFFmpeg(ctx, cpuArgs, totalSheets, onProgress); err != nil {
			return nil, fmt.Errorf("sprite generation failed: %w", err)
		}
	}

	var spriteFiles []string
	for i := 1; i <= 1000; i++ {
		name := fmt.Sprintf("sprite-%d.jpg", i)
		if _, err := os.Stat(filepath.Join(spriteDir, name)); err != nil {
			break
		}
		spriteFiles = append(spriteFiles, name)
	}
	if len(spriteFiles) == 0 {
		return nil, fmt.Errorf("no sprite files generated")
	}

	firstPath := filepath.Join(spriteDir, spriteFiles[0])
	if actualW, actualH := probeImageDimensions(firstPath); actualW > 0 && actualH > 0 {
		realW := actualW / spriteCols
		realH := actualH / spriteMaxRows
		if realW != thumbWidth || realH != thumbHeight {
			thumbWidth, thumbHeight = realW, realH
		}
	}

	// Keep every sheet at the same 6x6 dimensions. The tile filter pads unused
	// cells in the final sheet; cropping that padding changes the image's natural
	// size and makes players that scale thumbnails by sheet dimensions display a
	// frame that no longer matches the VTT xywh rectangle.

	vttContent := generateVTT(spriteFiles, spriteInterval, thumbWidth, thumbHeight, totalFrames, spriteCols, framesPerSheet)
	vttPath := filepath.Join(spriteDir, "sprite.vtt")
	if err := os.WriteFile(vttPath, []byte(vttContent), 0644); err != nil {
		return nil, fmt.Errorf("write sprite.vtt: %w", err)
	}
	spriteFiles = append(spriteFiles, "sprite.vtt")

	return &Result{
		SpriteDir:   spriteDir,
		SpriteFiles: spriteFiles,
		VTTFile:     "sprite.vtt",
	}, nil
}

func spriteFFmpegArgs(inputPath, spritePattern, filter string, cuda bool) []string {
	args := []string{
		"-y", "-hide_banner", "-loglevel", "error", "-nostats",
		"-stats_period", "0.5", "-progress", "pipe:1",
	}
	if cuda {
		args = append(args, "-hwaccel", "cuda", "-hwaccel_output_format", "cuda")
	}
	return append(args,
		"-i", inputPath,
		"-vf", filter,
		"-q:v", "9",
		spritePattern,
	)
}

var (
	nvidiaSpriteOnce      sync.Once
	nvidiaSpriteSupported bool
	nvidiaSpriteReason    string
)

// nvidiaSpriteAvailable checks the runtime, not just FFmpeg's compiled-in CUDA
// names. The real input can still use an unsupported codec, so Generate also
// keeps a per-job CPU fallback.
func nvidiaSpriteAvailable(enabled bool) (bool, string) {
	if !enabled {
		return false, "disabled"
	}
	nvidiaSpriteOnce.Do(func() {
		if output, err := exec.Command("nvidia-smi", "-L").CombinedOutput(); err != nil {
			nvidiaSpriteReason = strings.TrimSpace(string(output))
			if nvidiaSpriteReason == "" {
				nvidiaSpriteReason = err.Error()
			}
			return
		}
		filters, err := exec.Command("ffmpeg", "-hide_banner", "-filters").CombinedOutput()
		if err != nil || !strings.Contains(string(filters), "scale_cuda") {
			nvidiaSpriteReason = "FFmpeg scale_cuda filter not found"
			return
		}
		nvidiaSpriteSupported = true
		nvidiaSpriteReason = "NVIDIA CUDA"
	})
	return nvidiaSpriteSupported, nvidiaSpriteReason
}

func utilsLogGPU(inputPath string) {
	log.Printf("🎮 NVIDIA GPU detected — sprites use NVDEC + scale_cuda (%s)", filepath.Base(inputPath))
}

func removeSpriteOutputs(pattern string) {
	matches, _ := filepath.Glob(strings.Replace(pattern, "%d", "*", 1))
	for _, match := range matches {
		_ = os.Remove(match)
	}
}

// runSpriteFFmpeg consumes machine-readable progress. The tile filter produces
// one output frame per sheet, so frame/totalSheets is the actual job progress.
func runSpriteFFmpeg(ctx context.Context, args []string, totalSheets int, onProgress func(int)) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pipe progress: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("pipe stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	progressDone := make(chan error, 1)
	go func() {
		lastPercent := -1
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			key, value, ok := strings.Cut(scanner.Text(), "=")
			if !ok || key != "frame" || totalSheets <= 0 {
				continue
			}
			frame, parseErr := strconv.Atoi(strings.TrimSpace(value))
			if parseErr != nil {
				continue
			}
			percent := frame * 100 / totalSheets
			if percent > 99 {
				percent = 99
			}
			if percent != lastPercent && onProgress != nil {
				lastPercent = percent
				onProgress(percent)
			}
		}
		progressDone <- scanner.Err()
	}()

	stderrDone := make(chan struct{}, 1)
	lastLines := make([]string, 0, 10)
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 16*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			lastLines = append(lastLines, line)
			if len(lastLines) > 10 {
				lastLines = lastLines[1:]
			}
		}
		stderrDone <- struct{}{}
	}()

	progressErr := <-progressDone
	<-stderrDone
	waitErr := cmd.Wait()
	if waitErr != nil {
		return fmt.Errorf("%w: %s", waitErr, strings.Join(lastLines, "\n"))
	}
	if progressErr != nil {
		return fmt.Errorf("read progress: %w", progressErr)
	}
	if onProgress != nil {
		onProgress(100)
	}
	return nil
}

func calcThumbSize(inputPath string) (int, int) {
	const thumbHeight = 168
	info, err := ProbeVideoInfo(inputPath)
	if err != nil || info.Width <= 0 || info.Height <= 0 {
		w := thumbHeight * 16 / 9 // default 16:9
		if w%2 != 0 {
			w++
		}
		return w, thumbHeight
	}
	w := int(float64(info.Width) * float64(thumbHeight) / float64(info.Height))
	if w%2 != 0 {
		w++
	}
	return w, thumbHeight
}

func generateVTT(spriteFiles []string, interval float64, thumbW, thumbH, totalFrames, cols, framesPerSheet int) string {
	var sb strings.Builder
	sb.WriteString("WEBVTT\n\n")
	frameIndex := 0
	for _, fileName := range spriteFiles {
		remaining := totalFrames - frameIndex
		framesInSheet := framesPerSheet
		if remaining < framesPerSheet {
			framesInSheet = remaining
		}
		if framesInSheet <= 0 {
			break
		}
		for i := 0; i < framesInSheet; i++ {
			col := i % cols
			row := i / cols
			start := float64(frameIndex) * interval
			end := float64(frameIndex+1) * interval
			sb.WriteString(fmt.Sprintf("%s --> %s\n", formatVTTTime(start), formatVTTTime(end)))
			sb.WriteString(fmt.Sprintf("%s#xywh=%d,%d,%d,%d\n\n",
				fileName, col*thumbW, row*thumbH, thumbW, thumbH))
			frameIndex++
		}
	}
	return sb.String()
}

func formatVTTTime(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	h := int(seconds) / 3600
	m := (int(seconds) % 3600) / 60
	s := int(seconds) % 60
	ms := int((seconds - float64(int(seconds))) * 1000)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}

func probeImageDimensions(imagePath string) (int, int) {
	cmd := exec.Command("ffprobe",
		"-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height", "-of", "csv=s=x:p=0", imagePath)
	output, err := cmd.Output()
	if err != nil {
		return 0, 0
	}
	parts := strings.Split(strings.TrimSpace(string(output)), "x")
	if len(parts) != 2 {
		return 0, 0
	}
	w, _ := strconv.Atoi(parts[0])
	h, _ := strconv.Atoi(parts[1])
	return w, h
}

// GetFileSize returns file size in bytes.
func GetFileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// CheckFFmpeg verifies ffmpeg is available.
func CheckFFmpeg() error {
	cmd := exec.Command("ffmpeg", "-version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg not found in PATH")
	}
	return nil
}
