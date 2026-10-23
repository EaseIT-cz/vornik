package api

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// GitHub #412, code review round 2, G2 (found at: review): the quickstart's
// known_default_db_password list and the doctor's knownDefaultDBPasswords were
// two hand-kept copies. A value added to one only would let the quickstart
// keep (or write) a password the doctor then reports as ERROR, or the reverse.
// The doctor's map is the source of truth; this test fails when the shell
// copy differs from it. Design: 2026-10-21-installation-credential-hygiene-design.md §2.6.
func TestDoctor412_QuickstartKnownDefaultsMatchDoctor(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "deployments", "podman", "quickstart.sh"))
	if err != nil {
		t.Fatalf("read quickstart.sh: %v", err)
	}
	shell, err := shellKnownDefaults(src)
	if err != nil {
		t.Fatal(err)
	}
	var doctor []string
	for v := range knownDefaultDBPasswords {
		doctor = append(doctor, v)
	}
	sort.Strings(doctor)
	if strings.Join(shell, "|") != strings.Join(doctor, "|") {
		t.Fatalf("quickstart known_default_db_password list differs from the doctor's knownDefaultDBPasswords\n quickstart: %v\n doctor:     %v", shell, doctor)
	}
	for _, v := range doctor {
		if v != strings.ToLower(v) {
			t.Errorf("doctor entry %q is not lower-case; both sides compare lower-cased values", v)
		}
	}
}

// shellKnownDefaults returns the sorted values of every "... ) return 0 ;;" arm
// of known_default_db_password in a quickstart.sh source. It reads ALL arms, so
// a list split over several arms is compared whole (code review round 3, F3:
// reading only the first arm would pass while a later arm differed). The
// function body runs to the first line that is exactly "}", which is how the
// script closes every function.
func shellKnownDefaults(src []byte) ([]string, error) {
	fn := regexp.MustCompile(`(?s)\nknown_default_db_password\(\) \{\n(.*?)\n\}\n`).FindSubmatch(src)
	if fn == nil {
		return nil, fmt.Errorf("known_default_db_password() not found in quickstart.sh")
	}
	arms := regexp.MustCompile(`(?m)^[ \t]*([^()\n]+)\)[ \t]*return 0[ \t]*;;`).FindAllSubmatch(fn[1], -1)
	if len(arms) == 0 {
		return nil, fmt.Errorf("known_default_db_password has no `... ) return 0 ;;` case arm")
	}
	// Every "return 0" in the body must be one of those arms; anything else
	// (a return 0 this parser did not read) fails rather than pass unread.
	if n := strings.Count(string(fn[1]), "return 0"); n != len(arms) {
		return nil, fmt.Errorf("known_default_db_password has %d `return 0` but %d parsed case arms", n, len(arms))
	}
	var out []string
	for _, a := range arms {
		for _, v := range strings.Split(string(a[1]), "|") {
			out = append(out, strings.TrimSpace(v))
		}
	}
	sort.Strings(out)
	return out, nil
}

// The parser itself: a two-arm list is read whole, and an unparsed return 0
// is an error, not a silent pass.
func TestDoctor412_ShellKnownDefaultsParser(t *testing.T) {
	two := []byte("x\nknown_default_db_password() {\n  case \"$1\" in\n    b|a) return 0 ;;\n    c) return 0 ;;\n  esac\n  return 1\n}\ny() {\n}\n")
	got, err := shellKnownDefaults(two)
	if err != nil || strings.Join(got, "|") != "a|b|c" {
		t.Fatalf("two arms: got %v, %v; want [a b c]", got, err)
	}
	odd := []byte("x\nknown_default_db_password() {\n  case \"$1\" in\n    a) return 0 ;;\n  esac\n  [ \"$1\" = z ] && return 0\n  return 1\n}\n")
	if _, err := shellKnownDefaults(odd); err == nil {
		t.Fatal("an unparsed return 0 must be an error")
	}
}
