package release_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"text/template"
)

func sourceRoot(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...) //nolint:gosec // fixed test tools and disposable fixture arguments
	cmd.Dir = dir
	// Snapshot/build fixtures never publish and never consume credentials.
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GORELEASER_CURRENT_TAG=") && !strings.HasPrefix(e, "GORELEASER_PREVIOUS_TAG=") && !strings.HasPrefix(e, "GITHUB_TOKEN=") && !strings.HasPrefix(e, "HOMEBREW_TAP_GITHUB_TOKEN=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "GITHUB_TOKEN=snapshot-only", "HOMEBREW_TAP_GITHUB_TOKEN=snapshot-only")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", args[0], err, out)
	}
	return string(out)
}

func TestPinnedModelChecksumGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release helper uses the Unix release runner")
	}
	script := filepath.Join(sourceRoot(t), ".github", "scripts", "verify-model-assets.sh")
	for _, scenario := range []string{"valid", "corrupt", "missing", "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			var manifest strings.Builder
			for _, name := range []string{"model.onnx", "vocab.txt", "tokenizer.json"} {
				data := "fixture " + name
				writeFile(t, filepath.Join(dir, name), data)
				fmt.Fprintf(&manifest, "%x  %s\n", sha256.Sum256([]byte(data)), name)
			}
			switch scenario {
			case "corrupt":
				writeFile(t, filepath.Join(dir, "model.onnx"), "corruption")
			case "missing":
				if err := os.Remove(filepath.Join(dir, "vocab.txt")); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				manifest.WriteString(strings.Split(manifest.String(), "\n")[0] + "\n")
			}
			writeFile(t, filepath.Join(dir, "MANIFEST.txt"), manifest.String())
			out, err := exec.Command("sh", script, dir).CombinedOutput()
			if scenario == "valid" && err != nil {
				t.Fatalf("valid pinned files rejected: %v\n%s", err, out)
			}
			if scenario != "valid" && err == nil {
				t.Fatalf("%s pinned files accepted: %s", scenario, out)
			}
		})
	}
}

func TestModelTagAtHEADDoesNotBecomeBinaryVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release helper uses the Unix release runner")
	}
	root := sourceRoot(t)
	dir := t.TempDir()
	run(t, dir, "git", "init", "--quiet")
	run(t, dir, "git", "config", "user.name", "Release fixture")
	run(t, dir, "git", "config", "user.email", "release-fixture@example.invalid")
	run(t, dir, "git", "remote", "add", "origin", "https://github.com/example/application.git")
	run(t, dir, "git", "commit", "--allow-empty", "-qm", "Application")
	run(t, dir, "git", "tag", "v1.2.3")
	run(t, dir, "git", "commit", "--allow-empty", "-qm", "Model")
	run(t, dir, "git", "tag", "model-v9.8.7")
	config := filepath.Join(dir, "goreleaser.yml")
	run(t, dir, "sh", filepath.Join(root, ".github", "scripts", "goreleaser-config.sh"), filepath.Join(root, ".goreleaser.yml"), config)
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "- 'model-v9.8.7'") {
		t.Fatal("model tag at HEAD was not excluded exactly")
	}
	tool, err := exec.LookPath("goreleaser")
	if err != nil {
		t.Log("native GoReleaser version-selection probe skipped: tool unavailable; exclusion generation tested")
		return
	}
	if err := os.MkdirAll(filepath.Join(dir, "cmd", "guild"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.invalid/release-fixture\n\ngo 1.26.9\n")
	writeFile(t, filepath.Join(dir, "cmd", "guild", "main.go"), "package main\nvar version, commit, date string\nfunc main(){}\n")
	run(t, dir, tool, "build", "--snapshot", "--clean", "--single-target", "--skip=before", "--config", config)
	meta, err := os.ReadFile(filepath.Join(dir, "dist", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Tag     string `json:"tag"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(meta, &m); err != nil {
		t.Fatal(err)
	}
	if m.Tag != "v1.2.3" || m.Version != "1.2.4-next" {
		t.Fatalf("model tag leaked into binary snapshot version: %+v", m)
	}
}

func TestReleaseChannelPolicy(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(sourceRoot(t), ".goreleaser.yml"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	if !strings.Contains(source, "prerelease: auto") || !strings.Contains(source, "skip_upload: auto") {
		t.Fatal("RC release and cask must both recognize prerelease tags")
	}
	field := regexp.MustCompile(`(?m)^ {2}make_latest: (".*")$`).FindStringSubmatch(source)
	if len(field) != 2 {
		t.Fatal("explicit latest-channel policy missing")
	}
	expression, err := strconv.Unquote(field[1])
	if err != nil {
		t.Fatal(err)
	}
	policy, err := template.New("latest").Parse(expression)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ prerelease, want string }{{"", "true"}, {"rc.1", "false"}, {"beta.2", "false"}} {
		var out bytes.Buffer
		if err := policy.Execute(&out, map[string]string{"Prerelease": tc.prerelease}); err != nil {
			t.Fatal(err)
		}
		if out.String() != tc.want {
			t.Errorf("prerelease %q latest=%q want %q", tc.prerelease, out.String(), tc.want)
		}
	}
	workflow, err := os.ReadFile(filepath.Join(sourceRoot(t), ".github", "workflows", "build-model.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workflow), `branches: ["**"]`) || !strings.Contains(string(workflow), "'recipe/**'") || !strings.Contains(string(workflow), "workflow_dispatch:") || !strings.Contains(string(workflow), "schedule:") {
		t.Fatal("model trigger must preserve recipe branch/manual/schedule builds while excluding tag pushes")
	}
}
