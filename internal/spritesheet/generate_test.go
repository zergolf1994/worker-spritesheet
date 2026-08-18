package spritesheet

import (
	"slices"
	"strings"
	"testing"
)

func TestSpriteFFmpegArgsCUDA(t *testing.T) {
	filter := "fps=1/1.00,scale_cuda=300:168,hwdownload,format=nv12,tile=6x6"
	args := spriteFFmpegArgs("input.mp4", "sprite-%d.jpg", filter, true)
	joined := strings.Join(args, " ")
	for _, expected := range []string{"-hwaccel cuda", "-hwaccel_output_format cuda", "-progress pipe:1", filter} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("CUDA args missing %q: %s", expected, joined)
		}
	}
}

func TestSpriteFFmpegArgsCPU(t *testing.T) {
	args := spriteFFmpegArgs("input.mp4", "sprite-%d.jpg", "fps=1/1.00,scale=300:168,tile=6x6", false)
	if slices.Contains(args, "cuda") {
		t.Fatalf("CPU args unexpectedly contain CUDA: %v", args)
	}
}
