package protocol

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// StreamFileFromDisk reads a single file from srcPath, writes it to w, and returns (bytes, sha256Hex, error).
func StreamFileFromDisk(srcPath string, w io.Writer) (int64, string, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()

	hasher := sha256.New()
	mw := io.MultiWriter(w, hasher)
	n, err := io.Copy(mw, f)
	if err != nil {
		return n, "", err
	}
	return n, hex.EncodeToString(hasher.Sum(nil)), nil
}

// ReceiveFileToDisk reads up to size bytes (or until EOF if size < 0) from r, writes to dstPath,
// automatically creating parent directories, and returns (bytes, sha256Hex, error).
func ReceiveFileToDisk(dstPath string, r io.Reader, size int64) (int64, string, error) {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return 0, "", fmt.Errorf("create parent dir: %w", err)
	}
	f, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()

	hasher := sha256.New()
	mw := io.MultiWriter(f, hasher)
	var src io.Reader = r
	if size >= 0 {
		src = io.LimitReader(r, size)
	}
	n, err := io.Copy(mw, src)
	if err != nil {
		return n, "", err
	}
	return n, hex.EncodeToString(hasher.Sum(nil)), nil
}

// PackDirToStream packs a directory into a tar.gz stream written to w.
func PackDirToStream(srcDir string, w io.Writer) (int64, int, string, error) {
	hasher := sha256.New()
	mw := io.MultiWriter(w, hasher)
	gw := gzip.NewWriter(mw)
	tw := tar.NewWriter(gw)

	var totalBytes int64
	var fileCount int

	srcDir = filepath.Clean(srcDir)
	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			header.Name += "/"
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			n, copyErr := io.Copy(tw, f)
			f.Close()
			if copyErr != nil {
				return copyErr
			}
			totalBytes += n
			fileCount++
		}
		return nil
	})
	if err != nil {
		_ = tw.Close()
		_ = gw.Close()
		return totalBytes, fileCount, "", err
	}
	if err := tw.Close(); err != nil {
		_ = gw.Close()
		return totalBytes, fileCount, "", err
	}
	if err := gw.Close(); err != nil {
		return totalBytes, fileCount, "", err
	}
	return totalBytes, fileCount, hex.EncodeToString(hasher.Sum(nil)), nil
}

// UnpackDirFromStream extracts a tar.gz stream from r into dstDir.
func UnpackDirFromStream(dstDir string, r io.Reader) (int64, int, string, error) {
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return 0, 0, "", err
	}
	hasher := sha256.New()
	trStream := io.TeeReader(r, hasher)
	gr, err := gzip.NewReader(trStream)
	if err != nil {
		return 0, 0, "", fmt.Errorf("gzip reader: %w", err)
	}
	gr.Multistream(false)
	defer gr.Close()

	tr := tar.NewReader(gr)
	var totalBytes int64
	var fileCount int

	cleanDst := filepath.Clean(dstDir)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return totalBytes, fileCount, "", err
		}
		rel := filepath.FromSlash(hdr.Name)
		target := filepath.Join(cleanDst, rel)
		// Guard against zip-slip path traversal
		if !strings.HasPrefix(filepath.Clean(target)+string(filepath.Separator), cleanDst+string(filepath.Separator)) &&
			filepath.Clean(target) != cleanDst {
			return totalBytes, fileCount, "", fmt.Errorf("invalid archive path: %s", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return totalBytes, fileCount, "", err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return totalBytes, fileCount, "", err
			}
			mode := os.FileMode(hdr.Mode) & 0777
			if mode == 0 {
				mode = 0644
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return totalBytes, fileCount, "", err
			}
			n, copyErr := io.Copy(f, tr)
			f.Close()
			if copyErr != nil {
				return totalBytes, fileCount, "", copyErr
			}
			totalBytes += n
			fileCount++
		}
	}
	_, _ = io.Copy(io.Discard, gr)
	return totalBytes, fileCount, hex.EncodeToString(hasher.Sum(nil)), nil
}
