package clawhub

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/sharing"
)

// Bound decompressed bytes and entry counts BEFORE ProcessBundle writes files.
// Its older installer limits are intentionally larger than sharing's RPC cap.
func checkShareArchive(raw []byte) error {
	seen := make(map[string]bool)
	var total int64
	check := func(name string, size int64, regular bool) error {
		name = strings.TrimSuffix(name, "/")
		if !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\:\x00") || seen[name] {
			return errors.New("clawhub: unsafe or duplicate archive path")
		}
		seen[name] = true
		if len(seen) > sharing.MaxFiles+shareMetadataFileCount || size < 0 || size > sharing.MaxBytes || total > sharing.MaxBytes-size {
			return errors.New("clawhub: expanded package exceeds sharing limits")
		}
		if regular {
			total += size
		}
		return nil
	}
	if len(raw) >= len(gzipMagic) && string(raw[:len(gzipMagic)]) == gzipMagic {
		if err := checkShareTar(raw, check); err != nil {
			return err
		}
	} else {
		if err := checkShareZip(raw, check); err != nil {
			return err
		}
	}
	if !seen["manifest.yaml"] && (seen["handler.sh"] || seen["manifest.yaml.sig"]) {
		return errors.New("clawhub: upstream file collides with generated adapter files")
	}
	return nil
}

func checkShareTar(raw []byte, check func(string, int64, bool) error) error {
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(io.LimitReader(gz, sharing.MaxBytes+shareArchiveOverheadBytes))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeReg {
			return errors.New("clawhub: only regular files and directories are supported")
		}
		if err := check(h.Name, h.Size, h.Typeflag == tar.TypeReg); err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return err
		}
	}
	return nil
}

func checkShareZip(raw []byte, check func(string, int64, bool) error) error {

	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		if !f.Mode().IsRegular() && !f.FileInfo().IsDir() {
			return errors.New("clawhub: only regular files and directories are supported")
		}
		if f.UncompressedSize64 > sharing.MaxBytes {
			return errors.New("clawhub: expanded file exceeds sharing limit")
		}
		if err := check(f.Name, int64(f.UncompressedSize64), f.Mode().IsRegular()); err != nil {
			return err
		}
		if f.Mode().IsRegular() {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			n, readErr := io.Copy(io.Discard, io.LimitReader(rc, sharing.MaxBytes+1))
			closeErr := rc.Close()
			if readErr != nil {
				return readErr
			}
			if closeErr != nil {
				return closeErr
			}
			if n != int64(f.UncompressedSize64) {
				return errors.New("clawhub: expanded file size disagrees with archive")
			}
		}
	}
	return nil
}
