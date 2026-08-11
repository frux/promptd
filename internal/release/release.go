package release

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	versionPattern  = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	metadataPattern = regexp.MustCompile(`^[0-9A-Za-z._-]+$`)
)

type Options struct {
	Root       string
	Output     string
	Version    string
	Commit     string
	SourceDate time.Time
}

type Artifact struct {
	Architecture string
	ArchivePath  string
	ChecksumPath string
	SHA256       string
}

func Build(ctx context.Context, options Options) ([]Artifact, error) {
	if err := validateOptions(options); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(options.Output, 0o755); err != nil {
		return nil, fmt.Errorf("create release directory: %w", err)
	}
	temporary, err := os.MkdirTemp("", "promptd-release-")
	if err != nil {
		return nil, fmt.Errorf("create release workspace: %w", err)
	}
	defer os.RemoveAll(temporary)

	architectures := []string{"amd64", "arm64"}
	artifacts := make([]Artifact, 0, len(architectures))
	for _, architecture := range architectures {
		binaryPath := filepath.Join(temporary, "promptd-"+architecture)
		if err := buildBinary(ctx, options, architecture, binaryPath); err != nil {
			return nil, err
		}
		assetName := "promptd_linux_" + architecture + ".tar.gz"
		archivePath := filepath.Join(options.Output, assetName)
		checksum, err := writeArchive(archivePath, options.SourceDate, []archiveFile{
			{Name: "promptd", Path: binaryPath, Mode: 0o755},
			{Name: "LICENSE", Path: filepath.Join(options.Root, "LICENSE"), Mode: 0o644},
			{Name: "README.md", Path: filepath.Join(options.Root, "README.md"), Mode: 0o644},
		})
		if err != nil {
			return nil, fmt.Errorf("package %s: %w", architecture, err)
		}
		checksumPath := archivePath + ".sha256"
		if err := writeChecksum(checksumPath, checksum, assetName); err != nil {
			return nil, err
		}
		artifacts = append(artifacts, Artifact{
			Architecture: architecture,
			ArchivePath:  archivePath,
			ChecksumPath: checksumPath,
			SHA256:       checksum,
		})
	}
	return artifacts, nil
}

func validateOptions(options Options) error {
	if options.Root == "" || !filepath.IsAbs(options.Root) {
		return fmt.Errorf("repository root must be absolute")
	}
	if options.Output == "" || !filepath.IsAbs(options.Output) {
		return fmt.Errorf("release output must be absolute")
	}
	if !versionPattern.MatchString(options.Version) {
		return fmt.Errorf("release version %q must be a semantic v-prefixed tag", options.Version)
	}
	if !metadataPattern.MatchString(options.Commit) {
		return fmt.Errorf("release commit %q contains unsupported characters", options.Commit)
	}
	if options.SourceDate.IsZero() {
		return fmt.Errorf("release source date is required")
	}
	for _, name := range []string{"LICENSE", "README.md", "go.mod"} {
		if _, err := os.Stat(filepath.Join(options.Root, name)); err != nil {
			return fmt.Errorf("inspect repository file %s: %w", name, err)
		}
	}
	return nil
}

func buildBinary(ctx context.Context, options Options, architecture, output string) error {
	buildDate := options.SourceDate.UTC().Format(time.RFC3339)
	ldflags := strings.Join([]string{
		"-s",
		"-w",
		"-buildid=",
		"-X", "github.com/frux/promptd/internal/buildinfo.Version=" + options.Version,
		"-X", "github.com/frux/promptd/internal/buildinfo.Commit=" + options.Commit,
		"-X", "github.com/frux/promptd/internal/buildinfo.Date=" + buildDate,
	}, " ")
	command := exec.CommandContext(ctx,
		"go", "build",
		"-mod=readonly",
		"-buildvcs=false",
		"-trimpath",
		"-ldflags", ldflags,
		"-o", output,
		"./cmd/promptd",
	)
	command.Dir = options.Root
	command.Env = replaceEnvironment(os.Environ(), map[string]string{
		"CGO_ENABLED": "0",
		"GOOS":        "linux",
		"GOARCH":      architecture,
	})
	outputText, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build linux/%s: %w: %s", architecture, err, strings.TrimSpace(string(outputText)))
	}
	return nil
}

func replaceEnvironment(environment []string, values map[string]string) []string {
	result := make([]string, 0, len(environment)+len(values))
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := values[key]; !replaced {
			result = append(result, entry)
		}
	}
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	return result
}

type archiveFile struct {
	Name string
	Path string
	Mode int64
}

func writeArchive(path string, sourceDate time.Time, files []archiveFile) (string, error) {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".promptd-archive-*")
	if err != nil {
		return "", fmt.Errorf("create archive: %w", err)
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return "", fmt.Errorf("set archive mode: %w", err)
	}

	digest := sha256.New()
	compressed, err := gzip.NewWriterLevel(io.MultiWriter(temporary, digest), gzip.BestCompression)
	if err != nil {
		temporary.Close()
		return "", fmt.Errorf("initialize gzip: %w", err)
	}
	compressed.Header.ModTime = sourceDate.UTC()
	compressed.Header.OS = 255
	archive := tar.NewWriter(compressed)
	for _, file := range files {
		if err := appendFile(archive, file, sourceDate.UTC()); err != nil {
			archive.Close()
			compressed.Close()
			temporary.Close()
			return "", err
		}
	}
	if err := archive.Close(); err != nil {
		compressed.Close()
		temporary.Close()
		return "", fmt.Errorf("finish tar archive: %w", err)
	}
	if err := compressed.Close(); err != nil {
		temporary.Close()
		return "", fmt.Errorf("finish gzip archive: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", fmt.Errorf("sync archive: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close archive: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", fmt.Errorf("install archive: %w", err)
	}
	keep = true
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func appendFile(archive *tar.Writer, file archiveFile, sourceDate time.Time) error {
	input, err := os.Open(file.Path)
	if err != nil {
		return fmt.Errorf("open archive input %q: %w", file.Path, err)
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return fmt.Errorf("inspect archive input %q: %w", file.Path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("archive input %q is not a regular file", file.Path)
	}
	header := &tar.Header{
		Name:     file.Name,
		Mode:     file.Mode,
		Size:     info.Size(),
		ModTime:  sourceDate,
		Typeflag: tar.TypeReg,
		Format:   tar.FormatUSTAR,
	}
	if err := archive.WriteHeader(header); err != nil {
		return fmt.Errorf("write archive header for %q: %w", file.Name, err)
	}
	if _, err := io.Copy(archive, input); err != nil {
		return fmt.Errorf("write archive file %q: %w", file.Name, err)
	}
	return nil
}

func writeChecksum(path, checksum, assetName string) error {
	if _, err := hex.DecodeString(checksum); err != nil || len(checksum) != sha256.Size*2 {
		return fmt.Errorf("invalid SHA-256 checksum %q", checksum)
	}
	if strings.ContainsAny(assetName, "\x00\r\n/\\") {
		return fmt.Errorf("invalid asset name %q", assetName)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".promptd-checksum-*")
	if err != nil {
		return fmt.Errorf("create checksum: %w", err)
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return fmt.Errorf("set checksum mode: %w", err)
	}
	if _, err := fmt.Fprintf(temporary, "%s  %s\n", checksum, assetName); err != nil {
		temporary.Close()
		return fmt.Errorf("write checksum: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close checksum: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install checksum: %w", err)
	}
	keep = true
	return nil
}
