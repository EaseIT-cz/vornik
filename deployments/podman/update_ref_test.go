package podman

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #64 (2026-10-03): --ref main selected a stale local branch and
// downgraded a customer's daemon into a crash loop. Execute the actual updater
// against real Git histories; stub only the service/container boundary.
func TestUpdaterRefAndDowngrade(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	origin := filepath.Join(dir, "origin.git")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--bare", origin)
	git("init", "-b", "main", repo)
	git("-C", repo, "config", "user.email", "test@example.invalid")
	git("-C", repo, "config", "user.name", "test")
	git("-C", repo, "commit", "--allow-empty", "-m", "old")
	old := git("-C", repo, "rev-parse", "HEAD")
	git("-C", repo, "tag", "2026.10.1")
	git("-C", repo, "remote", "add", "origin", origin)
	git("-C", repo, "commit", "--allow-empty", "-m", "new")
	newCommit := git("-C", repo, "rev-parse", "HEAD")
	git("-C", repo, "push", "origin", "main")
	git("-C", repo, "tag", "release-branch", old)
	git("-C", repo, "push", "origin", "main:release-branch")
	git("-C", repo, "push", "origin", "main:feature/fix")
	git("-C", repo, "checkout", "--detach", old)
	git("-C", repo, "branch", "-f", "main", old)
	git("-C", repo, "checkout", "-b", "divergent")
	git("-C", repo, "commit", "--allow-empty", "-m", "divergent")
	divergent := git("-C", repo, "rev-parse", "HEAD")
	git("-C", repo, "checkout", "--detach", old)
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(name, []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(bin, "podman"), "#!/bin/bash\ncase \"$*\" in *'ps --format'*) echo vornik-postgres;; *psql*) echo 214;; esac\n")
	serviceLog := filepath.Join(dir, "service.log")
	write(filepath.Join(bin, "systemctl"), "#!/bin/bash\nprintf '%s\\n' \"$*\" >> '"+serviceLog+"'\nexit 0\n")
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(bin, "git"), "#!/bin/bash\nif [[ \"$*\" == *merge-base* && ${ANCESTRY_FAIL:-} == 1 ]]; then exit 128; fi\nexec '"+gitPath+"' \"$@\"\n")
	write(filepath.Join(dir, "config.yaml"), "listen: ':8080'\n")
	updater, err := filepath.Abs("vornik-update.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, ref, installed, target, message string
		flags                                 []string
		refuse                                bool
		ancestryFail                          bool
	}{
		{name: "stale main uses remote", ref: "main", installed: newCommit, target: "refs/remotes/origin/main"},
		{name: "branch with slash", ref: "feature/fix", installed: newCommit, target: "refs/remotes/origin/feature/fix"},
		{name: "explicit local branch refuses", ref: "refs/heads/main", installed: newCommit, refuse: true},
		{name: "tag refuses", ref: "2026.10.1", installed: newCommit, refuse: true},
		{name: "force does not bypass", ref: old, installed: newCommit, flags: []string{"--force", "--yes"}, refuse: true},
		{name: "allow downgrade", ref: old, installed: newCommit, flags: []string{"--allow-downgrade"}, message: "downgrade"},
		{name: "newer target", ref: "main", installed: old, target: "refs/remotes/origin/main"},
		{name: "unknown build", ref: "main", installed: "dev", message: "downgrade safety could not be verified"},
		{name: "unavailable installed commit", ref: "main", installed: strings.Repeat("f", 40), message: "downgrade safety could not be verified"},
		{name: "tag wins branch collision", ref: "release-branch", installed: newCommit, refuse: true},
		{name: "divergent is not ancestor", ref: divergent, installed: newCommit},
		{name: "git inspection fails closed", ref: old, installed: newCommit, refuse: true, ancestryFail: true, message: "ancestry inspection failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version := "2026.10.3-1-g" + tc.installed
			if tc.installed == "dev" {
				version = "dev"
			}
			write(filepath.Join(bin, "vornik"), "#!/bin/bash\necho 'vornik "+version+"'\n")
			args := append([]string{updater, "--check", "--ref", tc.ref}, tc.flags...)
			cmd := exec.Command("bash", args...)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "VORNIK_DIR="+repo, "VORNIK_CONFIG="+filepath.Join(dir, "config.yaml"), "VORNIK_BIN_DIR="+bin, "VORNIK_UPDATE_REEXEC=", "VORNIK_UPDATE_COPY_DIR=", "ANCESTRY_FAIL=0")
			if tc.ancestryFail {
				cmd.Env = append(cmd.Env, "ANCESTRY_FAIL=1")
			}
			out, runErr := cmd.CombinedOutput()
			if tc.refuse != (runErr != nil) {
				t.Fatalf("refuse=%v, err=%v: %s", tc.refuse, runErr, out)
			}
			if tc.refuse && !tc.ancestryFail && !strings.Contains(string(out), "--allow-downgrade") {
				t.Fatalf("missing actionable refusal: %s", out)
			}
			if tc.target != "" && !strings.Contains(string(out), "target  : "+tc.target+" ") {
				t.Fatalf("wrong resolved target: %s", out)
			}
			if tc.message != "" && !strings.Contains(string(out), tc.message) {
				t.Fatalf("missing %q: %s", tc.message, out)
			}
			if strings.Contains(string(out), "Backing up") {
				t.Fatalf("preflight must precede backup: %s", out)
			}
			if got := git("-C", repo, "rev-parse", "HEAD"); got != old {
				t.Fatalf("checkout mutated: %s", got)
			}
			calls, readErr := os.ReadFile(serviceLog)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, call := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
				if call != "--user cat vornik.service" {
					t.Fatalf("service mutated: %s", call)
				}
			}
		})
	}
}
