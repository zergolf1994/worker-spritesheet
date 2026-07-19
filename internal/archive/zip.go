package archive

import (
	"archive/zip"
	"os"
	"path/filepath"
)

// ZipDir creates a zip archive from all files in srcDir (flat, no subdirs) —
// โหมด remote: อัด sprite/ เป็น sprite.zip ก่อนอัพ S3 ให้ worker-transfer ติดตั้งต่อ.
func ZipDir(srcDir, zipPath string) error {
	out, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer out.Close()

	w := zip.NewWriter(out)
	defer w.Close()

	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(srcDir, e.Name()))
		if err != nil {
			return err
		}
		f, err := w.Create(e.Name())
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			return err
		}
	}
	return nil
}
