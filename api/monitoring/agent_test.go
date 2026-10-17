package monitoring

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestPinnedImage(t *testing.T) {
	for _, image := range []string{"ghcr.io/acme/monitor:v1.2.3", "ghcr.io/acme/monitor@sha256:abc"} {
		if !pinnedImage(image) {
			t.Fatalf("expected pinned image: %s", image)
		}
	}
	for _, image := range []string{"ghcr.io/acme/monitor", "ghcr.io/acme/monitor:latest"} {
		if pinnedImage(image) {
			t.Fatalf("expected unpinned image: %s", image)
		}
	}
}

func TestValidateConfigRequiresBothTokens(t *testing.T) {
	config := Config{Image: "ghcr.io/acme/monitor:v1", HostPort: 9184, RetentionDays: 7, ControlToken: "control"}
	if err := ValidateConfig(config); err == nil {
		t.Fatal("expected missing reader token error")
	}
}

func TestBuildRunCommand(t *testing.T) {
	command := buildRunCommand("sudo -n docker", Config{
		Image:         "ghcr.io/acme/monitor:v1",
		HostPort:      9184,
		RetentionDays: 7,
		ControlToken:  "control's token",
		ReaderToken:   "reader token",
	}, "/tmp/stagezero-monitoring/abc-1/env")

	for _, want := range []string{
		"sudo -n docker run -d --name uploy-monitor",
		"--network stagezero",
		"-p 127.0.0.1:9184:9184",
		"-v /proc:/host/proc:ro -v /sys:/host/sys:ro -v /:/host:ro",
		"--env-file '/tmp/stagezero-monitoring/abc-1/env'",
		"UPLOY_MONITOR_RETENTION_DAYS=7 -e HOST_PROC=/host/proc -e HOST_SYS=/host/sys -e HOST_ROOT=/host",
		"'ghcr.io/acme/monitor:v1'",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("buildRunCommand() = %q; missing %q", command, want)
		}
	}
	// Tokens travel in the env file; anything token-shaped on this line is the
	// disclosure the --env-file shape exists to prevent.
	for _, forbidden := range []string{"control's token", "control'\\''s token", "reader token", "UPLOY_MONITOR_CONTROL_TOKEN", "UPLOY_MONITOR_READER_TOKEN"} {
		if strings.Contains(command, forbidden) {
			t.Fatalf("buildRunCommand() = %q; leaked %q", command, forbidden)
		}
	}
}

type commandResult struct {
	output string
	err    error
}

type sequenceRunner struct {
	results  []commandResult
	commands []string
}

func (r *sequenceRunner) DockerBin() string {
	return "docker"
}

func (r *sequenceRunner) Run(_ context.Context, command string) (string, error) {
	r.commands = append(r.commands, command)
	if len(r.results) == 0 {
		return "", nil
	}
	result := r.results[0]
	r.results = r.results[1:]
	return result.output, result.err
}

func TestEnableStateCleanup(t *testing.T) {
	t.Run("restores a previously running container", func(t *testing.T) {
		runner := &sequenceRunner{results: []commandResult{{output: "id"}, {}, {}}}
		state := enableState{
			oldMoved:      true,
			oldWasRunning: true,
			newStarted:    true,
			rollbackName:  ContainerName + "-rollback",
		}

		if err := state.cleanup(context.Background(), runner, false); err != nil {
			t.Fatalf("cleanup() error = %v", err)
		}
		want := []string{
			"docker container ls --all --filter 'name=^/uploy-monitor$' --format '{{.ID}}'",
			"docker rm -f 'uploy-monitor'",
			"docker rename uploy-monitor-rollback uploy-monitor && docker start uploy-monitor",
		}
		if got := strings.Join(runner.commands, "\n"); got != strings.Join(want, "\n") {
			t.Fatalf("cleanup commands = %q; want %q", runner.commands, want)
		}
	})

	t.Run("removes the rollback container after success", func(t *testing.T) {
		runner := &sequenceRunner{results: []commandResult{{output: "id"}, {}}}
		state := enableState{oldMoved: true, rollbackName: ContainerName + "-rollback"}

		if err := state.cleanup(context.Background(), runner, true); err != nil {
			t.Fatalf("cleanup() error = %v", err)
		}
		want := []string{
			"docker container ls --all --filter 'name=^/uploy-monitor-rollback$' --format '{{.ID}}'",
			"docker rm -f 'uploy-monitor-rollback'",
		}
		if got := strings.Join(runner.commands, "\n"); got != strings.Join(want, "\n") {
			t.Fatalf("cleanup commands = %q; want %q", runner.commands, want)
		}
	})

	t.Run("removes a newly started container when no old one existed", func(t *testing.T) {
		runner := &sequenceRunner{results: []commandResult{{output: "id"}, {}}}
		state := enableState{newStarted: true}

		if err := state.cleanup(context.Background(), runner, false); err != nil {
			t.Fatalf("cleanup() error = %v", err)
		}
		want := []string{
			"docker container ls --all --filter 'name=^/uploy-monitor$' --format '{{.ID}}'",
			"docker rm -f 'uploy-monitor'",
		}
		if got := strings.Join(runner.commands, "\n"); got != strings.Join(want, "\n") {
			t.Fatalf("cleanup commands = %q; want %q", runner.commands, want)
		}
	})

	t.Run("restarts an old container stopped before rename", func(t *testing.T) {
		runner := &sequenceRunner{}
		state := enableState{oldStopped: true}

		if err := state.cleanup(context.Background(), runner, false); err != nil {
			t.Fatalf("cleanup() error = %v", err)
		}
		if got, want := runner.commands, []string{"docker start uploy-monitor"}; strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("cleanup commands = %q; want %q", got, want)
		}
	})

	t.Run("returns cleanup failures", func(t *testing.T) {
		runErr := errors.New("ssh disconnected")
		runner := &sequenceRunner{results: []commandResult{{err: runErr}, {}}}
		state := enableState{oldMoved: true, rollbackName: ContainerName + "-rollback"}

		if err := state.cleanup(context.Background(), runner, false); !errors.Is(err, runErr) {
			t.Fatalf("cleanup() error = %v; want wrapped %v", err, runErr)
		}
	})
}

// The agent must never be published on a routable address: loopback plus the SSH
// tunnel is the whole reason no firewall rule is needed to run it.
func TestBuildRunCommandPublishesOnLoopbackOnly(t *testing.T) {
	command := buildRunCommand("docker", Config{
		Image:         "ghcr.io/acme/monitor:v1",
		HostPort:      9500,
		RetentionDays: 7,
		ControlToken:  "control",
		ReaderToken:   "reader",
	}, "/tmp/stagezero-monitoring/abc-1/env")
	if !strings.Contains(command, "-p 127.0.0.1:9500:9184") {
		t.Fatalf("buildRunCommand() = %q; want loopback publish on the configured port", command)
	}
	if strings.Contains(command, "-p 0.0.0.0:") {
		t.Fatalf("buildRunCommand() = %q; must not bind a routable address", command)
	}
}

type envStep struct {
	command string
	input   []byte
	ctxErr  error
}

// recordingRunner stands in for the SSH client: it captures the commands the
// env-file flow issues, and optionally fails either call.
type recordingRunner struct {
	steps     []envStep
	runErr    error
	uploadErr error
}

func (r *recordingRunner) DockerBin() string { return "docker" }

func (r *recordingRunner) Run(ctx context.Context, command string) (string, error) {
	r.steps = append(r.steps, envStep{command: command, ctxErr: ctx.Err()})
	return "", r.runErr
}

func (r *recordingRunner) RunWithInput(ctx context.Context, command string, input []byte) (string, error) {
	r.steps = append(r.steps, envStep{command: command, input: input, ctxErr: ctx.Err()})
	return "", r.uploadErr
}

func envTestConfig() Config {
	return Config{
		Image:         "ghcr.io/acme/monitor:v1",
		HostPort:      9184,
		RetentionDays: 7,
		ControlToken:  "control-secret-value",
		ReaderToken:   "reader-secret-value",
	}
}

func TestWriteAgentEnvFileUploadsSecretsOnStdinOnly(t *testing.T) {
	cfg := envTestConfig()
	runner := &recordingRunner{}
	dir, err := writeAgentEnvFile(context.Background(), runner, cfg)
	if err != nil {
		t.Fatalf("writeAgentEnvFile() error = %v", err)
	}
	if !strings.HasPrefix(dir, agentEnvRoot+"/") {
		t.Fatalf("env dir = %q; want a directory under %s", dir, agentEnvRoot)
	}
	if len(runner.steps) != 3 {
		t.Fatalf("steps = %#v; want prepare, verify and upload", runner.steps)
	}
	upload := runner.steps[2]
	if got, want := string(upload.input), agentEnvContent(cfg); got != want {
		t.Fatalf("uploaded stdin = %q; want %q", got, want)
	}
	for _, step := range runner.steps {
		for _, secret := range []string{cfg.ControlToken, cfg.ReaderToken} {
			if strings.Contains(step.command, secret) {
				t.Fatalf("command %q carries the secret %q", step.command, secret)
			}
		}
	}
	if !strings.Contains(upload.command, "umask 077") || !strings.Contains(upload.command, "cat > ") {
		t.Fatalf("upload command = %q; want a 0600 write", upload.command)
	}
	if !strings.Contains(runner.steps[0].command, "-mmin +60") {
		t.Fatalf("prepare command = %q; want stale-dir pruning", runner.steps[0].command)
	}
	if !strings.Contains(runner.steps[1].command, "stat -c %a") || !strings.Contains(runner.steps[1].command, "stat -c %u") {
		t.Fatalf("verify command = %q; want an ownership and mode check", runner.steps[1].command)
	}
}

func TestWriteAgentEnvFileUsesDistinctDirectoryPerOperation(t *testing.T) {
	runner := &recordingRunner{}
	first, err := writeAgentEnvFile(context.Background(), runner, envTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	second, err := writeAgentEnvFile(context.Background(), runner, envTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("both operations used %q; concurrent cleanup would delete the other's file", first)
	}
}

func TestWriteAgentEnvFileRemovesDirectoryWhenUploadFails(t *testing.T) {
	runner := &recordingRunner{uploadErr: errors.New("ssh disconnected")}
	if _, err := writeAgentEnvFile(context.Background(), runner, envTestConfig()); err == nil {
		t.Fatal("expected upload failure")
	}
	last := runner.steps[len(runner.steps)-1]
	if !strings.HasPrefix(last.command, "rm -rf ") {
		t.Fatalf("steps = %#v; want the failed upload cleaned up", runner.steps)
	}
}

// Cancellation is exactly when a secret file is most likely to be stranded, so
// the removal must run even though the request's context is already done.
func TestRemoveAgentEnvFileSurvivesCancellation(t *testing.T) {
	runner := &recordingRunner{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := removeAgentEnvFile(ctx, runner, agentEnvRoot+"/abc-1"); err != nil {
		t.Fatalf("removeAgentEnvFile() error = %v", err)
	}
	if len(runner.steps) != 1 || runner.steps[0].command != "rm -rf "+agentEnvRoot+"/abc-1" {
		t.Fatalf("steps = %#v; want one removal", runner.steps)
	}
	if runner.steps[0].ctxErr != nil {
		t.Fatalf("removal ran with a cancelled context: %v", runner.steps[0].ctxErr)
	}
}

func TestValidateConfigRejectsTokenLineBreaks(t *testing.T) {
	cfg := envTestConfig()
	cfg.ReaderToken = "reader-secret\nUPLOY_MONITOR_RETENTION_DAYS=0"
	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("accepted a token that would inject an environment entry")
	}
}

// executingRunner runs the recorded commands locally, which is the only way to
// check claims about the real filesystem: permissions and cleanup cannot be
// proven by inspecting a string.
type executingRunner struct {
	recordingRunner
}

func (r *executingRunner) Run(ctx context.Context, command string) (string, error) {
	r.steps = append(r.steps, envStep{command: command, ctxErr: ctx.Err()})
	out, err := exec.Command("sh", "-c", command).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (r *executingRunner) RunWithInput(ctx context.Context, command string, input []byte) (string, error) {
	r.steps = append(r.steps, envStep{command: command, input: input, ctxErr: ctx.Err()})
	commandline := exec.Command("sh", "-c", command)
	commandline.Stdin = bytes.NewReader(input)
	out, err := commandline.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// The upload runs for real here against a temporary HOME, so the same paths the
// SSH session would use resolve to a throwaway directory.
func TestAgentEnvFileIsRestrictedOnHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := envTestConfig()
	runner := &executingRunner{}
	dir, err := writeAgentEnvFile(context.Background(), runner, cfg)
	if err != nil {
		t.Fatalf("writeAgentEnvFile() error = %v", err)
	}
	local := home + "/.stagezero-monitoring/" + strings.TrimPrefix(dir, agentEnvRoot+"/")

	info, err := os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("directory mode = %o; want 700", perm)
	}
	fileInfo, err := os.Stat(local + "/env")
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("env file mode = %o; want 600", perm)
	}
	content, err := os.ReadFile(local + "/env")
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != agentEnvContent(cfg) {
		t.Fatalf("env file = %q", content)
	}

	if err := removeAgentEnvFile(context.Background(), runner, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("env directory survived cleanup: %v", err)
	}
}

// A directory another user could have created must stop the operation instead of
// receiving the tokens.
func TestAgentEnvFileRejectsUnsafeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(home+"/.stagezero-monitoring", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := writeAgentEnvFile(context.Background(), &executingRunner{}, envTestConfig()); err == nil {
		t.Fatal("accepted a world-readable environment directory")
	}
	entries, err := os.ReadDir(home + "/.stagezero-monitoring")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("wrote into an untrusted directory: %v", entries)
	}
}

// An upload that fails must also remove the directory the file was meant for.
func TestAgentEnvFileRemovesDirectoryWhenUploadFailsOnHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	runner := &failingUploadRunner{}
	if _, err := writeAgentEnvFile(context.Background(), runner, envTestConfig()); err == nil {
		t.Fatal("expected upload failure")
	}
	entries, err := os.ReadDir(home + "/.stagezero-monitoring")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed upload left %v behind", entries)
	}
}

type failingUploadRunner struct {
	executingRunner
}

func (r *failingUploadRunner) RunWithInput(context.Context, string, []byte) (string, error) {
	return "", errors.New("ssh disconnected")
}
