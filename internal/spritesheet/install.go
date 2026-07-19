package spritesheet

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// installFile moves a single file into {storagePath}/{fileID}/{fileName}.
func installFile(storagePath, fileID, fileName, srcPath string) error {
	destDir := filepath.Join(storagePath, fileID)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}
	return moveFile(srcPath, filepath.Join(destDir, fileName))
}

// installDir moves all files from srcDir into {storagePath}/{fileID}/{subDir}/.
func installDir(storagePath, fileID, subDir, srcDir string) error {
	destDir := filepath.Join(storagePath, fileID, subDir)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := moveFile(filepath.Join(srcDir, e.Name()), filepath.Join(destDir, e.Name())); err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
	}
	return nil
}

// moveFile renames when possible (same volume), falls back to copy+delete.
func moveFile(src, dest string) error {
	if err := os.Rename(src, dest); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	out.Close()
	return os.Remove(src)
}
