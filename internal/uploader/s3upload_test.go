package uploader

import "testing"

func TestContentTypeForSpriteAssets(t *testing.T) {
	tests := map[string]string{
		"sprite.vtt":   "text/vtt; charset=utf-8",
		"sprite-1.jpg": "image/jpeg",
		"sprite.zip":   "application/zip",
	}
	for fileName, want := range tests {
		if got := contentTypeFor(fileName); got != want {
			t.Errorf("contentTypeFor(%q) = %q, want %q", fileName, got, want)
		}
	}
}
