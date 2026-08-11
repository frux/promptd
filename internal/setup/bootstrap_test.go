package setup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapInstallerVerifiesAndInstallsRelease(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	asset := "promptd_linux_amd64.tar.gz"
	for _, test := range []struct {
		name          string
		validChecksum bool
		wantSuccess   bool
	}{
		{name: "valid", validChecksum: true, wantSuccess: true},
		{name: "invalid checksum", validChecksum: false, wantSuccess: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			fixtures := filepath.Join(directory, "fixtures")
			fakeBin := filepath.Join(directory, "bin")
			installDir := filepath.Join(directory, "install")
			if err := os.MkdirAll(fixtures, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(fakeBin, 0o700); err != nil {
				t.Fatal(err)
			}

			payload := []byte("#!/bin/sh\necho fake promptd\n")
			archivePath := filepath.Join(fixtures, asset)
			writeReleaseArchive(t, archivePath, payload)
			archive, err := os.ReadFile(archivePath)
			if err != nil {
				t.Fatal(err)
			}
			checksum := fmt.Sprintf("%x", sha256.Sum256(archive))
			if !test.validChecksum {
				checksum = strings.Repeat("0", 64)
			}
			if err := os.WriteFile(filepath.Join(fixtures, asset+".sha256"), []byte(checksum+"  "+asset+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			writeTestExecutable(t, filepath.Join(fakeBin, "uname"), `#!/bin/sh
case "$1" in
    -s) echo Linux ;;
    -m) echo x86_64 ;;
    *) exit 1 ;;
esac
`)
			writeTestExecutable(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
destination=
url=
while [ "$#" -gt 0 ]; do
    case "$1" in
        -o) destination=$2; shift 2 ;;
        http*) url=$1; shift ;;
        *) shift ;;
    esac
done
[ -n "$destination" ] && [ -n "$url" ] || exit 2
cp "$FIXTURE_DIR/${url##*/}" "$destination"
`)

			command := exec.Command("sh", filepath.Join(root, "install.sh"))
			command.Env = []string{
				"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
				"HOME=" + directory,
				"FIXTURE_DIR=" + fixtures,
				"PROMPTD_INSTALL_DIR=" + installDir,
				"PROMPTD_RELEASE_BASE_URL=https://example.invalid/releases",
				"PROMPTD_SETUP=skip",
			}
			output, runErr := command.CombinedOutput()
			if test.wantSuccess && runErr != nil {
				t.Fatalf("install.sh error = %v\n%s", runErr, output)
			}
			if !test.wantSuccess {
				if runErr == nil || !strings.Contains(string(output), "checksum verification failed") {
					t.Fatalf("install.sh error = %v, output = %s", runErr, output)
				}
				if _, err := os.Stat(filepath.Join(installDir, "promptd")); !os.IsNotExist(err) {
					t.Fatalf("invalid release was installed: %v", err)
				}
				return
			}

			installed, err := os.ReadFile(filepath.Join(installDir, "promptd"))
			if err != nil {
				t.Fatal(err)
			}
			if string(installed) != string(payload) {
				t.Fatalf("installed payload = %q", installed)
			}
			info, err := os.Stat(filepath.Join(installDir, "promptd"))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm()&0o111 == 0 {
				t.Fatalf("installed mode = %v", info.Mode())
			}
		})
	}
}

func writeReleaseArchive(t *testing.T, path string, payload []byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: "promptd", Mode: 0o755, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeTestExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}
