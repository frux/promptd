package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildPlanSelectsUserSystemdAndNumericLinger(t *testing.T) {
	environment := testEnvironment()
	plan, err := BuildPlan(environment, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != ModeUserSystemd || plan.RequiresRoot {
		t.Fatalf("plan = %#v", plan)
	}
	if len(plan.Files) != 2 || plan.Files[1].Path != "/home/virtual/.config/systemd/user/promptd.service" {
		t.Fatalf("files = %#v", plan.Files)
	}
	unit := string(plan.Files[1].Content)
	for _, expected := range []string{
		managedMarker,
		`ExecStart="/home/virtual/.local/bin/promptd" "daemon"`,
		`Environment="HOME=/home/virtual"`,
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, expected) {
			t.Fatalf("unit does not contain %q:\n%s", expected, unit)
		}
	}
	if strings.Contains(unit, "User=") {
		t.Fatalf("user unit changes identity:\n%s", unit)
	}
	if len(plan.Commands) != 4 {
		t.Fatalf("commands = %#v", plan.Commands)
	}
	wantLinger := []string{"/usr/bin/loginctl", "enable-linger", "35232"}
	if strings.Join(plan.Commands[0].Args, " ") != strings.Join(wantLinger, " ") {
		t.Fatalf("linger command = %#v", plan.Commands[0].Args)
	}
}

func TestBuildPlanSkipsAlreadyEnabledLinger(t *testing.T) {
	environment := testEnvironment()
	environment.LingerEnabled = true
	plan, err := BuildPlan(environment, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Commands) != 3 || strings.Contains(strings.Join(plan.Commands[0].Args, " "), "loginctl") {
		t.Fatalf("commands = %#v", plan.Commands)
	}
}

func TestBuildPlanSelectsSystemModeForRoot(t *testing.T) {
	environment := testEnvironment()
	environment.UID = 0
	environment.GID = 0
	environment.EUID = 0
	options := testOptions()
	options.AllowRoot = true
	plan, err := BuildPlan(environment, options)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != ModeSystemSystemd || !plan.RequiresRoot {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.Files[1].Path != "/etc/systemd/system/promptd.service" {
		t.Fatalf("unit path = %q", plan.Files[1].Path)
	}
	if !plan.Files[0].Chown || plan.Files[0].UID != 0 || plan.Files[0].GID != 0 {
		t.Fatalf("config ownership = %#v", plan.Files[0])
	}
	unit := string(plan.Files[1].Content)
	for _, expected := range []string{"User=0", "Group=0", "WantedBy=multi-user.target"} {
		if !strings.Contains(unit, expected) {
			t.Fatalf("unit does not contain %q:\n%s", expected, unit)
		}
	}
}

func TestBuildPlanRefusesRootAgentByDefault(t *testing.T) {
	environment := testEnvironment()
	environment.UID = 0
	environment.GID = 0
	environment.EUID = 0
	if _, err := BuildPlan(environment, testOptions()); err == nil || !strings.Contains(err.Error(), "as root") {
		t.Fatalf("BuildPlan() error = %v", err)
	}
}

func TestBuildPlanFallsBackToPortable(t *testing.T) {
	environment := testEnvironment()
	environment.UserManagerAvailable = false
	plan, err := BuildPlan(environment, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != ModePortable || len(plan.Files) != 1 || len(plan.Commands) != 1 {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.Commands[0].Args[1] != "daemon" {
		t.Fatalf("portable command = %#v", plan.Commands[0])
	}
}

func TestBuildPlanRejectsUnavailableExplicitUserSystemd(t *testing.T) {
	environment := testEnvironment()
	environment.UserManagerAvailable = false
	options := testOptions()
	options.Mode = ModeUserSystemd
	if _, err := BuildPlan(environment, options); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("BuildPlan() error = %v", err)
	}
}

func TestSystemdUnitEscapesArgumentsAndSpecifiers(t *testing.T) {
	environment := testEnvironment()
	environment.Path = `/usr/local/$tools:/opt/100%/bin`
	options := testOptions()
	options.BinaryPath = `/home/virtual/bin/prompt$d`
	options.ConfigPath = `/home/virtual/config 100%/say"hi.yaml`
	plan, err := BuildPlan(environment, options)
	if err != nil {
		t.Fatal(err)
	}
	unit := string(plan.Files[1].Content)
	for _, expected := range []string{
		`"/home/virtual/bin/prompt$$d"`,
		`"/home/virtual/config 100%%/say\"hi.yaml"`,
		`Environment="PATH=/usr/local/$tools:/opt/100%%/bin"`,
	} {
		if !strings.Contains(unit, expected) {
			t.Fatalf("unit does not contain %q:\n%s", expected, unit)
		}
	}
}

func TestApplyPortableCreatesPrivateConfigWithoutOverwriting(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config", "config.yaml")
	environment := testEnvironment()
	environment.GOOS = "darwin"
	options := testOptions()
	options.ConfigPath = configPath
	options.Mode = ModePortable
	plan, err := BuildPlan(environment, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o", info.Mode().Perm())
	}
	directoryInfo, err := os.Stat(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("config directory mode = %o", directoryInfo.Mode().Perm())
	}
	if err := os.WriteFile(configPath, []byte("custom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(configPath)
	if err != nil || string(contents) != "custom\n" {
		t.Fatalf("config overwritten: contents=%q error=%v", contents, err)
	}
}

func TestApplyFileProtectsUnmanagedUnit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "promptd.service")
	if err := os.WriteFile(path, []byte("custom unit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := File{
		Path: path, Content: []byte(managedMarker + "\nnew\n"), Mode: 0o644, DirectoryMode: 0o755, Managed: true,
	}
	if err := applyFile(file); err == nil || !strings.Contains(err.Error(), "unmanaged") {
		t.Fatalf("applyFile() error = %v", err)
	}
	file.Force = true
	if err := applyFile(file); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != string(file.Content) {
		t.Fatalf("contents=%q error=%v", contents, err)
	}
}

func testEnvironment() Environment {
	return Environment{
		GOOS:                 "linux",
		UID:                  35232,
		GID:                  35232,
		EUID:                 35232,
		HomeDir:              "/home/virtual",
		ConfigHome:           "/home/virtual/.config",
		Path:                 "/usr/local/bin:/usr/bin:/bin",
		Executable:           "/home/virtual/.local/bin/promptd",
		Systemctl:            "/usr/bin/systemctl",
		Loginctl:             "/usr/bin/loginctl",
		UserManagerAvailable: true,
		LingerKnown:          true,
	}
}

func testOptions() Options {
	return Options{
		Mode:         ModeAuto,
		ConfigPath:   "/home/virtual/.config/promptd/config.yaml",
		StatePath:    "/home/virtual/.local/state/promptd/promptd.db",
		LogDir:       "/home/virtual/.local/state/promptd/logs",
		SocketPath:   "/home/virtual/.local/state/promptd/promptd.sock",
		EnableLinger: true,
	}
}
