package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const managedMarker = "# Managed by promptd setup."

type Mode string

const (
	ModeAuto          Mode = "auto"
	ModeUserSystemd   Mode = "user-systemd"
	ModeSystemSystemd Mode = "system-systemd"
	ModePortable      Mode = "portable"
)

type Environment struct {
	GOOS                 string
	UID                  int
	GID                  int
	EUID                 int
	HomeDir              string
	ConfigHome           string
	Path                 string
	Executable           string
	Systemctl            string
	Loginctl             string
	UserManagerAvailable bool
	LingerKnown          bool
	LingerEnabled        bool
}

type Options struct {
	Mode         Mode
	BinaryPath   string
	ConfigPath   string
	StatePath    string
	LogDir       string
	SocketPath   string
	EnableLinger bool
	Force        bool
	AllowRoot    bool
}

type File struct {
	Path          string
	Content       []byte
	Mode          os.FileMode
	DirectoryMode os.FileMode
	IfMissing     bool
	Managed       bool
	Force         bool
	Chown         bool
	UID           int
	GID           int
}

type Command struct {
	Args        []string
	Description string
}

type Plan struct {
	Mode         Mode
	Reason       string
	BinaryPath   string
	Files        []File
	Commands     []Command
	Notes        []string
	RequiresRoot bool
}

func Inspect(ctx context.Context) (Environment, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Environment{}, fmt.Errorf("resolve home directory: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return Environment{}, fmt.Errorf("resolve promptd executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return Environment{}, fmt.Errorf("resolve promptd executable: %w", err)
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}

	uid, gid, euid := currentIdentity()
	environment := Environment{
		GOOS:       runtime.GOOS,
		UID:        uid,
		GID:        gid,
		EUID:       euid,
		HomeDir:    home,
		ConfigHome: configHome,
		Path:       os.Getenv("PATH"),
		Executable: executable,
	}
	environment.Systemctl, _ = exec.LookPath("systemctl")
	environment.Loginctl, _ = exec.LookPath("loginctl")
	if environment.GOOS != "linux" || environment.Systemctl == "" {
		return environment, nil
	}

	probeContext, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if err := exec.CommandContext(probeContext, environment.Systemctl, "--user", "show-environment").Run(); err == nil {
		environment.UserManagerAvailable = true
	}
	if environment.Loginctl == "" {
		return environment, nil
	}
	output, err := exec.CommandContext(
		probeContext,
		environment.Loginctl,
		"show-user",
		strconv.Itoa(environment.UID),
		"--property=Linger",
		"--value",
	).Output()
	if err == nil {
		environment.LingerKnown = true
		environment.LingerEnabled = strings.EqualFold(strings.TrimSpace(string(output)), "yes")
	}
	return environment, nil
}

func BuildPlan(environment Environment, options Options) (Plan, error) {
	if options.Mode == "" {
		options.Mode = ModeAuto
	}
	mode, reason, err := selectMode(environment, options.Mode)
	if err != nil {
		return Plan{}, err
	}
	if options.BinaryPath == "" {
		options.BinaryPath = environment.Executable
	}
	for label, path := range map[string]string{
		"binary": options.BinaryPath,
		"config": options.ConfigPath,
		"state":  options.StatePath,
		"logs":   options.LogDir,
		"socket": options.SocketPath,
	} {
		if path == "" {
			return Plan{}, fmt.Errorf("%s path is empty", label)
		}
		if !filepath.IsAbs(path) {
			return Plan{}, fmt.Errorf("%s path must be absolute: %q", label, path)
		}
		if strings.ContainsAny(path, "\x00\r\n") {
			return Plan{}, fmt.Errorf("%s path contains a control character", label)
		}
	}
	if environment.HomeDir == "" || !filepath.IsAbs(environment.HomeDir) {
		return Plan{}, fmt.Errorf("home directory must be absolute")
	}
	if mode == ModeUserSystemd && !filepath.IsAbs(environment.ConfigHome) {
		return Plan{}, fmt.Errorf("user configuration directory must be absolute")
	}

	plan := Plan{Mode: mode, Reason: reason, BinaryPath: options.BinaryPath}
	configFile := File{
		Path:          options.ConfigPath,
		Content:       []byte("version: 1\n\njobs: {}\n"),
		Mode:          0o600,
		DirectoryMode: 0o700,
		IfMissing:     true,
	}
	if mode == ModeSystemSystemd {
		configFile.Chown = true
		configFile.UID = environment.UID
		configFile.GID = environment.GID
	}
	plan.Files = append(plan.Files, configFile)

	if mode == ModePortable {
		plan.Notes = append(plan.Notes,
			"No supported service manager was selected; promptd will not start automatically.",
			"Run the printed daemon command under a container runtime or another supervisor.",
		)
		if environment.GOOS == "linux" && environment.Systemctl != "" && environment.EUID != 0 {
			plan.Notes = append(plan.Notes, "System-wide systemd remains available via --mode system-systemd when setup is run as root.")
		}
		plan.Commands = append(plan.Commands, Command{
			Description: "run promptd in the foreground",
			Args:        daemonArguments(options),
		})
		return plan, nil
	}
	if mode == ModeSystemSystemd && environment.UID == 0 && !options.AllowRoot {
		return Plan{}, fmt.Errorf("refusing to run scheduled agents as root; set a non-root service UID or pass --allow-root explicitly")
	}

	unit, err := renderSystemdUnit(environment, options, mode)
	if err != nil {
		return Plan{}, err
	}
	unitPath := "/etc/systemd/system/promptd.service"
	directoryMode := os.FileMode(0o755)
	if mode == ModeUserSystemd {
		unitPath = filepath.Join(environment.ConfigHome, "systemd", "user", "promptd.service")
	}
	plan.Files = append(plan.Files, File{
		Path:          unitPath,
		Content:       unit,
		Mode:          0o644,
		DirectoryMode: directoryMode,
		Managed:       true,
		Force:         options.Force,
	})

	if mode == ModeUserSystemd {
		if options.EnableLinger && !environment.LingerEnabled {
			if environment.Loginctl != "" {
				plan.Commands = append(plan.Commands, Command{
					Description: "keep the user systemd manager active after logout",
					Args:        []string{environment.Loginctl, "enable-linger", strconv.Itoa(environment.UID)},
				})
			} else {
				plan.Notes = append(plan.Notes, "loginctl is unavailable; enable lingering manually if jobs must run after logout.")
			}
		}
		if !environment.LingerKnown && options.EnableLinger {
			plan.Notes = append(plan.Notes, "The current linger state could not be determined; enabling it may require administrator approval.")
		}
		plan.Commands = append(plan.Commands,
			Command{Description: "reload user units", Args: []string{environment.Systemctl, "--user", "daemon-reload"}},
			Command{Description: "enable promptd at user-manager startup", Args: []string{environment.Systemctl, "--user", "enable", "promptd.service"}},
			Command{Description: "start or restart promptd", Args: []string{environment.Systemctl, "--user", "restart", "promptd.service"}},
		)
		return plan, nil
	}

	plan.RequiresRoot = true
	plan.Commands = append(plan.Commands,
		Command{Description: "reload system units", Args: []string{environment.Systemctl, "daemon-reload"}},
		Command{Description: "enable promptd at boot", Args: []string{environment.Systemctl, "enable", "promptd.service"}},
		Command{Description: "start or restart promptd", Args: []string{environment.Systemctl, "restart", "promptd.service"}},
	)
	return plan, nil
}

func selectMode(environment Environment, requested Mode) (Mode, string, error) {
	switch requested {
	case ModeAuto:
		switch {
		case environment.GOOS != "linux":
			return ModePortable, "systemd integration is Linux-only", nil
		case environment.Systemctl == "":
			return ModePortable, "systemctl was not found", nil
		case environment.EUID == 0:
			return ModeSystemSystemd, "running as root with systemd available", nil
		case environment.UserManagerAvailable:
			return ModeUserSystemd, "the current user systemd manager is reachable", nil
		default:
			return ModePortable, "the current user systemd manager is not reachable", nil
		}
	case ModeUserSystemd:
		if environment.GOOS != "linux" || environment.Systemctl == "" {
			return "", "", fmt.Errorf("user-systemd mode requires Linux and systemctl")
		}
		if !environment.UserManagerAvailable {
			return "", "", fmt.Errorf("user-systemd manager is not reachable")
		}
		return requested, "selected explicitly", nil
	case ModeSystemSystemd:
		if environment.GOOS != "linux" || environment.Systemctl == "" {
			return "", "", fmt.Errorf("system-systemd mode requires Linux and systemctl")
		}
		return requested, "selected explicitly", nil
	case ModePortable:
		return requested, "selected explicitly", nil
	default:
		return "", "", fmt.Errorf("unknown setup mode %q", requested)
	}
}

func renderSystemdUnit(environment Environment, options Options, mode Mode) ([]byte, error) {
	arguments := daemonArguments(options)
	escaped := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		value, err := quoteExecArgument(argument)
		if err != nil {
			return nil, err
		}
		escaped = append(escaped, value)
	}
	home, err := quoteEnvironment("HOME=" + environment.HomeDir)
	if err != nil {
		return nil, err
	}
	var output strings.Builder
	output.WriteString(managedMarker + "\n")
	output.WriteString("[Unit]\n")
	output.WriteString("Description=promptd scheduled AI agent daemon\n")
	output.WriteString("Documentation=https://github.com/frux/promptd\n")
	output.WriteString("Wants=network-online.target\n")
	output.WriteString("After=network-online.target\n\n")
	output.WriteString("[Service]\n")
	output.WriteString("Type=simple\n")
	if mode == ModeSystemSystemd {
		output.WriteString("User=" + strconv.Itoa(environment.UID) + "\n")
		output.WriteString("Group=" + strconv.Itoa(environment.GID) + "\n")
	}
	output.WriteString("Environment=" + home + "\n")
	if environment.Path != "" {
		path, err := quoteEnvironment("PATH=" + environment.Path)
		if err != nil {
			return nil, err
		}
		output.WriteString("Environment=" + path + "\n")
	}
	output.WriteString("ExecStart=" + strings.Join(escaped, " ") + "\n")
	output.WriteString("ExecReload=kill -HUP $MAINPID\n")
	output.WriteString("Restart=on-failure\n")
	output.WriteString("RestartSec=5s\n")
	output.WriteString("TimeoutStopSec=45s\n")
	output.WriteString("KillMode=control-group\n")
	output.WriteString("UMask=0077\n\n")
	output.WriteString("[Install]\n")
	if mode == ModeUserSystemd {
		output.WriteString("WantedBy=default.target\n")
	} else {
		output.WriteString("WantedBy=multi-user.target\n")
	}
	return []byte(output.String()), nil
}

func daemonArguments(options Options) []string {
	return []string{
		options.BinaryPath,
		"daemon",
		"--config", options.ConfigPath,
		"--state", options.StatePath,
		"--log-dir", options.LogDir,
		"--socket", options.SocketPath,
	}
}

func quoteExecArgument(value string) (string, error) {
	quoted, err := quote(value)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(quoted, "$", "$$"), nil
}

func quoteEnvironment(value string) (string, error) {
	return quote(value)
}

func quote(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("systemd value contains a control character")
	}
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	value = strings.ReplaceAll(value, "%", "%%")
	return "\"" + value + "\"", nil
}

const probeTimeout = 3 * time.Second

func Apply(ctx context.Context, plan Plan) error {
	if plan.RequiresRoot && !hasRootPrivileges() {
		return fmt.Errorf("%s setup must be applied as root", plan.Mode)
	}
	if plan.Mode != ModePortable {
		info, err := os.Stat(plan.BinaryPath)
		if err != nil {
			return fmt.Errorf("inspect promptd binary: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("promptd binary %q is not an executable regular file", plan.BinaryPath)
		}
	}
	for _, file := range plan.Files {
		if err := applyFile(file); err != nil {
			return err
		}
	}
	if plan.Mode == ModePortable {
		return nil
	}
	for _, command := range plan.Commands {
		process := exec.CommandContext(ctx, command.Args[0], command.Args[1:]...)
		var output bytes.Buffer
		process.Stdout = &output
		process.Stderr = &output
		if err := process.Run(); err != nil {
			message := strings.TrimSpace(output.String())
			if message == "" {
				return fmt.Errorf("%s: %w", command.Description, err)
			}
			return fmt.Errorf("%s: %w: %s", command.Description, err, message)
		}
	}
	return nil
}

func applyFile(file File) error {
	if file.Path == "" || !filepath.IsAbs(file.Path) {
		return fmt.Errorf("installation file path must be absolute: %q", file.Path)
	}
	existing, err := os.ReadFile(file.Path)
	switch {
	case err == nil:
		if bytes.Equal(existing, file.Content) || file.IfMissing {
			return nil
		}
		if file.Managed && !file.Force && !bytes.HasPrefix(existing, []byte(managedMarker)) {
			return fmt.Errorf("refusing to overwrite unmanaged file %q; use --force to replace it", file.Path)
		}
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("inspect installation file %q: %w", file.Path, err)
	}

	if err := ensureDirectory(filepath.Dir(file.Path), file.DirectoryMode, file.Chown, file.UID, file.GID); err != nil {
		return fmt.Errorf("create directory for %q: %w", file.Path, err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(file.Path), ".promptd-setup-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %q: %w", file.Path, err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(file.Mode); err != nil {
		temporary.Close()
		return fmt.Errorf("set mode for %q: %w", file.Path, err)
	}
	if file.Chown {
		if err := temporary.Chown(file.UID, file.GID); err != nil {
			temporary.Close()
			return fmt.Errorf("set owner for %q: %w", file.Path, err)
		}
	}
	if _, err := temporary.Write(file.Content); err != nil {
		temporary.Close()
		return fmt.Errorf("write %q: %w", file.Path, err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync %q: %w", file.Path, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close %q: %w", file.Path, err)
	}
	if err := os.Rename(temporaryPath, file.Path); err != nil {
		return fmt.Errorf("install %q: %w", file.Path, err)
	}
	removeTemporary = false
	return nil
}

func ensureDirectory(path string, mode os.FileMode, chown bool, uid, gid int) error {
	var missing []string
	current := path
	for {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%q is not a directory", current)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			return fmt.Errorf("cannot find an existing parent for %q", path)
		}
		current = parent
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	for index := len(missing) - 1; index >= 0; index-- {
		created := missing[index]
		if err := os.Chmod(created, mode); err != nil {
			return err
		}
		if chown {
			if err := os.Chown(created, uid, gid); err != nil {
				return err
			}
		}
	}
	return nil
}
