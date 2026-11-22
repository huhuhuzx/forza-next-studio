package assets

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const bundleVersion = "forzanext-v3-single-exe-1"

//go:embed web/*
var embeddedWeb embed.FS

//go:embed presets/*
var embeddedPresets embed.FS

//go:embed v4_bundle.tar.gz
var v4BundleTarGz []byte

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

// EnsureV4Runtime extracts the embedded V4 runtime (EXE + DLL + diagnostics) into targetDir
// if missing or outdated. Any existing user runtime_config.json is preserved!
func EnsureV4Runtime(targetDir string) error {
	stampPath := filepath.Join(targetDir, ".bundle_version")
	exePath := filepath.Join(targetDir, "ForzaTelemetryHarness.exe")
	dllPath := filepath.Join(targetDir, "forza_telemetry_algorithm.dll")

	if stamp, err := os.ReadFile(stampPath); err == nil && strings.TrimSpace(string(stamp)) == bundleVersion {
		if _, err1 := os.Stat(exePath); err1 == nil {
			if _, err2 := os.Stat(dllPath); err2 == nil {
				return nil
			}
		}
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("create v4 dir: %w", err)
	}

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
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			continue
		}
		dst := filepath.Join(targetDir, clean)
		if hdr.Typeflag == tar.TypeDir {
			_ = os.MkdirAll(dst, 0755)
			continue
		}
		// Never overwrite an already-customized user runtime_config.json!
		if strings.EqualFold(filepath.ToSlash(clean), "config/runtime_config.json") {
			if _, statErr := os.Stat(dst); statErr == nil {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0644); err != nil {
			return fmt.Errorf("extract %s: %w", clean, err)
		}
	}
	return os.WriteFile(stampPath, []byte(bundleVersion+"\n"), 0644)
}
