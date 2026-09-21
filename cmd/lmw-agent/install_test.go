package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/config"
)

func TestInstallWritesEnvironmentUnitAndUserDropIn(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "etc")
	stateRoot := filepath.Join(root, "state")
	systemdRoot := filepath.Join(root, "systemd")
	unitSource := filepath.Join(root, "source.service")
	if err := os.WriteFile(unitSource, []byte("[Service]\nExecStart=/usr/local/bin/lmw-agent run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvConfigDir, configDir)
	t.Setenv(config.EnvAgentStateRoot, stateRoot)
	t.Setenv("LMW_SYSTEMD_ROOT", systemdRoot)
	t.Setenv("LMW_AGENT_UNIT_SOURCE", unitSource)
	var output bytes.Buffer
	token := strings.Repeat("a", 64)
	ca := strings.Repeat("b", 64)
	err := install([]string{
		"--server", "https://lmw.example.test:9443",
		"--ca-sha256", ca,
		"--token", token,
		"--run-as", "operator",
		"--cache-root", "/srv/models",
		"--cache-root", "/home/operator/.cache/huggingface",
	}, &output)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	env, err := os.ReadFile(filepath.Join(configDir, "agent.env"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(env)
	for _, want := range []string{
		config.EnvAgentServer + "=https://lmw.example.test:9443",
		config.EnvAgentCASha256 + "=" + ca,
		config.EnvAgentToken + "=" + token,
		config.EnvAgentWorkspace + "=" + filepath.Join(stateRoot, "workspace"),
		config.EnvAgentCacheRoots + "=/srv/models:/home/operator/.cache/huggingface",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("agent.env missing %q:\n%s", want, text)
		}
	}
	info, err := os.Stat(filepath.Join(configDir, "agent.env"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("agent.env mode = %v, err=%v", info.Mode().Perm(), err)
	}
	unit, err := os.ReadFile(filepath.Join(systemdRoot, "local-model-works-agent.service"))
	if err != nil || !strings.Contains(string(unit), "ExecStart=/usr/local/bin/lmw-agent run") {
		t.Fatalf("installed unit = %q, err=%v", unit, err)
	}
	dropIn, err := os.ReadFile(filepath.Join(systemdRoot, "local-model-works-agent.service.d", "10-run-as.conf"))
	if err != nil || string(dropIn) != "[Service]\nUser=operator\n" {
		t.Fatalf("drop-in = %q, err=%v", dropIn, err)
	}
	cacheDropIn, err := os.ReadFile(filepath.Join(systemdRoot, "local-model-works-agent.service.d", "20-cache-roots.conf"))
	if err != nil {
		t.Fatal(err)
	}
	wantCacheDropIn := "[Service]\nReadWritePaths=\"/srv/models\"\nReadWritePaths=\"/home/operator/.cache/huggingface\"\n"
	if string(cacheDropIn) != wantCacheDropIn {
		t.Fatalf("cache-root drop-in = %q, want %q", cacheDropIn, wantCacheDropIn)
	}
}

func TestInstallRejectsUnsafeInputs(t *testing.T) {
	valid := []string{
		"--server", "https://lmw.example.test:9443",
		"--ca-sha256", strings.Repeat("b", 64),
		"--token", strings.Repeat("a", 64),
		"--run-as", "operator",
	}
	cases := [][]string{
		append(append([]string{}, valid...), "--cache-root", "../escape"),
		{"--server", "http://lmw.example.test:9443", "--ca-sha256", strings.Repeat("b", 64), "--token", strings.Repeat("a", 64), "--run-as", "operator"},
		{"--server", "https://lmw.example.test:9443", "--ca-sha256", "bad", "--token", strings.Repeat("a", 64), "--run-as", "operator"},
	}
	for _, args := range cases {
		if err := install(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("unsafe args accepted: %v", args)
		}
	}
}

func TestInstallHostPolicyRequiresReviewedChoiceAndPreservesDisabledWorkers(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "etc")
	systemdRoot := filepath.Join(root, "systemd")
	unitSource, err := filepath.Abs(filepath.Join("..", "..", "deploy", "systemd", "local-model-works-agent.service"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvConfigDir, configDir)
	t.Setenv(config.EnvAgentStateRoot, filepath.Join(root, "state"))
	t.Setenv("LMW_SYSTEMD_ROOT", systemdRoot)
	t.Setenv("LMW_AGENT_UNIT_SOURCE", unitSource)
	valid := []string{"--server", "https://lmw.example.test:9443", "--ca-sha256", strings.Repeat("b", 64), "--token", strings.Repeat("a", 64), "--run-as", "operator"}
	run := func(extra ...string) error {
		t.Helper()
		return install(append(append([]string{}, valid...), extra...), &bytes.Buffer{})
	}
	if err := run("--upstream-execution"); err == nil {
		t.Fatal("host execution accepted without service authority review")
	}
	if _, err := os.Stat(configDir); !os.IsNotExist(err) {
		t.Fatalf("rejected authority choice wrote configuration: %v", err)
	}
	if err := run("--upstream-execution", "--upstream-host-policy=host", "--dry-run"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configDir); !os.IsNotExist(err) {
		t.Fatalf("installer preview wrote configuration: %v", err)
	}
	if _, err := os.Stat(systemdRoot); !os.IsNotExist(err) {
		t.Fatalf("installer preview wrote service policy: %v", err)
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	hostDropIn := filepath.Join(systemdRoot, "local-model-works-agent.service.d", "30-upstream-host.conf")
	if _, err := os.Stat(hostDropIn); !os.IsNotExist(err) {
		t.Fatalf("default install relaxed host policy: %v", err)
	}
	if err := run("--upstream-execution", "--upstream-host-policy=host"); err != nil {
		t.Fatal(err)
	}
	host, err := os.ReadFile(hostDropIn)
	if err != nil {
		t.Fatal(err)
	}
	// These installed systemd settings are the observable authority/restart
	// contract, not incidental template formatting.
	settings := map[string]string{}
	for _, line := range strings.Split(string(host), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(key, "#") {
			settings[key] = value
		}
	}
	for _, key := range []string{"NoNewPrivileges", "PrivateTmp", "ProtectSystem", "ProtectHome", "ProtectKernelTunables", "ProtectKernelModules", "ProtectControlGroups", "RestrictSUIDSGID", "LockPersonality"} {
		if settings[key] != "false" {
			t.Fatalf("installed host policy still restricts %s: %q", key, settings[key])
		}
	}
	if settings["KillMode"] != "process" {
		t.Fatal("installed host policy would kill approved supervisors on agent restart")
	}
	if err := run(); err == nil {
		t.Fatal("implicit reinstall silently revoked worker survival policy")
	}
	if err := run("--upstream-execution=false", "--upstream-host-policy=host"); err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile(filepath.Join(configDir, "agent.env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), config.EnvAgentUpstreamExecution+"=false\n") {
		t.Fatal("installer did not revoke new host execution")
	}
	if _, err := os.Stat(hostDropIn); err != nil {
		t.Fatalf("disabling launches removed existing worker policy: %v", err)
	}
	if err := run("--upstream-host-policy=hardened"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hostDropIn); !os.IsNotExist(err) {
		t.Fatalf("explicit hardened choice did not revoke host service policy: %v", err)
	}
}
