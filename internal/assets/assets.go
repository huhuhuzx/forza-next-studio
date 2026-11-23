package assets

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed web/*
var embeddedWeb embed.FS

//go:embed presets/*
var embeddedPresets embed.FS

//go:embed v4_bundle.tar.gz
var v4BundleTarGz []byte

var (
	ensureV4Mu sync.Mutex

	bundleVersion = func() string {
		sum := sha256.Sum256(v4BundleTarGz)
		return "sha256-" + hex.EncodeToString(sum[:])
	}()

	expectedV4Files = []string{
		"ForzaTelemetryHarness.exe",
		"forza_telemetry_algorithm.dll",
		"forza_telemetry_algorithm.manifest.json",
		"forza_telemetry_algorithm.provenance.json",
		"config/default_config.json",
		"config/runtime_config.json",
		"monitor/diagnostics.html",
		"monitor/icons.svg",
	}
)

// WebFS returns the embedded Web Studio static files (index.html, app.js, app.css).
func WebFS() fs.FS {
	sub, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		panic(err)
	}
	return sub
}

// PresetsFS returns the embedded 11 V4 Pro Mode preset JSON files + manifest.json.
func PresetsFS() fs.FS {
	sub, err := fs.Sub(embeddedPresets, "presets")
	if err != nil {
		panic(err)
	}
	return sub
}

func verifyV4RuntimeFiles(absTarget string) bool {
	for _, rel := range expectedV4Files {
		st, err := os.Stat(filepath.Join(absTarget, filepath.FromSlash(rel)))
		if err != nil || st.IsDir() || st.Size() <= 0 {
			return false
		}
	}
	return true
}

func safeExtractPath(absTarget, rawName string) (dst string, rel string, ok bool) {
	if rawName == "" {
		return "", "", false
	}
	norm := strings.ReplaceAll(rawName, "\\", "/")
	if strings.HasPrefix(norm, "/") || filepath.VolumeName(norm) != "" || filepath.IsAbs(norm) {
		return "", "", false
	}
	clean := filepath.Clean(filepath.FromSlash(norm))
	if clean == "." || clean == ".." || filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" {
		return "", "", false
	}
	dst = filepath.Join(absTarget, clean)
	rel, err := filepath.Rel(absTarget, dst)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", "", false
	}
	return dst, rel, true
}

func atomicWriteFile(dst string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".v4tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmpPath, perm)
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// EnsureV4Runtime extracts the embedded V4 runtime (EXE + DLL + diagnostics) into targetDir
// if missing or outdated. Any existing non-empty user runtime_config.json is preserved!
func EnsureV4Runtime(targetDir string) error {
	ensureV4Mu.Lock()
	defer ensureV4Mu.Unlock()

	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		return fmt.Errorf("resolve v4 dir: %w", err)
	}
	stampPath := filepath.Join(absTarget, ".bundle_version")

	if stamp, err := os.ReadFile(stampPath); err == nil && strings.TrimSpace(string(stamp)) == bundleVersion {
		if verifyV4RuntimeFiles(absTarget) {
			return nil
		}
	}

	if err := os.MkdirAll(absTarget, 0755); err != nil {
		return fmt.Errorf("create v4 dir: %w", err)
	}
	_ = os.Remove(stampPath)

	gz, err := gzip.NewReader(bytes.NewReader(v4BundleTarGz))
	if err != nil {
		return fmt.Errorf("open v4 bundle gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read v4 bundle tar: %w", err)
		}
		dst, rel, ok := safeExtractPath(absTarget, hdr.Name)
		if !ok {
			continue
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(dst, 0755); err != nil {
				return fmt.Errorf("mkdir %s: %w", rel, err)
			}
			continue
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		// Never overwrite an already-customized non-empty user runtime_config.json!
		if strings.EqualFold(filepath.ToSlash(rel), "config/runtime_config.json") {
			if st, statErr := os.Stat(dst); statErr == nil && !st.IsDir() && st.Size() > 0 {
				continue
			}
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("read %s: %w", rel, err)
		}
		if err := atomicWriteFile(dst, data, 0644); err != nil {
			return fmt.Errorf("extract %s: %w", rel, err)
		}
	}
	if !verifyV4RuntimeFiles(absTarget) {
		return fmt.Errorf("v4 runtime bundle verification failed in %s", absTarget)
	}
	return atomicWriteFile(stampPath, []byte(bundleVersion+"\n"), 0644)
}
