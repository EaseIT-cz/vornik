// Package opsapi carries the operations API documents: the contract a hosting
// control plane calls on a managed instance's operations listener (GitHub
// #575; https://docs.vornik.io).
// Each served major is one embedded OpenAPI document, v<N>.yaml; a release
// serves the newest major and, when there are two, the one before it.
package opsapi

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed v*.yaml
var documents embed.FS

var majorFile = regexp.MustCompile(`^v([1-9][0-9]*)\.yaml$`)

// Served returns the majors this release serves, sorted: every embedded
// document. GET /api/v1/capabilities reports exactly this list.
func Served() []string {
	entries, err := fs.ReadDir(documents, ".")
	if err != nil {
		return nil
	}
	var majors []string
	for _, e := range entries {
		if m := majorFile.FindStringSubmatch(e.Name()); m != nil {
			majors = append(majors, "v"+m[1])
		}
	}
	sortMajors(majors)
	return majors
}

// Document returns a served major's document.
func Document(major string) ([]byte, bool) {
	b, err := documents.ReadFile(major + ".yaml")
	if err != nil {
		return nil, false
	}
	return b, true
}

func majorNumber(major string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(major, "v"))
	if err != nil || !strings.HasPrefix(major, "v") {
		return -1
	}
	return n
}

func sortMajors(majors []string) {
	sort.Slice(majors, func(i, j int) bool { return majorNumber(majors[i]) < majorNumber(majors[j]) })
}

// CheckServed applies the N/N-1 rule: the newest major and, when there is more
// than one, exactly the one before it.
func CheckServed(majors []string) error {
	switch len(majors) {
	case 0:
		return errors.New("no operations API major is served")
	case 1:
		if majorNumber(majors[0]) < 1 {
			return fmt.Errorf("%q is not a major", majors[0])
		}
		return nil
	case 2:
		a, b := majorNumber(majors[0]), majorNumber(majors[1])
		if a < 1 || b != a+1 {
			return fmt.Errorf("majors %v: a release serves the newest major and the one before it", majors)
		}
		return nil
	default:
		return fmt.Errorf("majors %v: a release serves at most two majors", majors)
	}
}

// Operation is one documented method and path with its success statuses.
type Operation struct {
	Method  string
	Path    string
	Success []string
}

func (o Operation) String() string { return o.Method + " " + o.Path }

var methods = []string{"get", "put", "post", "delete", "patch", "head", "options"}

// Methods are the OpenAPI operation keys Operations reads, so a test that walks
// a document reads the same set.
func Methods() []string { return append([]string(nil), methods...) }

// Operations is the structural parse of a document: the OpenAPI 3.1 top-level
// keys and every operation with at least one response. It is not a full
// OpenAPI validation.
func Operations(doc []byte) ([]Operation, error) {
	var d struct {
		OpenAPI string `yaml:"openapi"`
		Info    struct {
			Title   string `yaml:"title"`
			Version string `yaml:"version"`
		} `yaml:"info"`
		Paths map[string]map[string]struct {
			Responses map[string]yaml.Node `yaml:"responses"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(doc, &d); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if !strings.HasPrefix(d.OpenAPI, "3.1") || d.Info.Title == "" || d.Info.Version == "" || len(d.Paths) == 0 {
		return nil, errors.New("not an OpenAPI 3.1 document with info.title, info.version and paths")
	}
	var ops []Operation
	for path, item := range d.Paths {
		for _, m := range methods {
			op, ok := item[m]
			if !ok {
				continue
			}
			if len(op.Responses) == 0 {
				return nil, fmt.Errorf("%s %s has no responses", strings.ToUpper(m), path)
			}
			o := Operation{Method: strings.ToUpper(m), Path: path}
			for code := range op.Responses {
				if strings.HasPrefix(code, "2") {
					o.Success = append(o.Success, code)
				}
			}
			if len(o.Success) == 0 {
				return nil, fmt.Errorf("%s has no success status", o)
			}
			sort.Strings(o.Success)
			ops = append(ops, o)
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].String() < ops[j].String() })
	return ops, nil
}

// Compatible reports what the current document of a major dropped from a
// released copy of it: an operation, or a documented success status. Request
// and response shapes are not compared.
func Compatible(current, released []byte) error {
	cur, err := Operations(current)
	if err != nil {
		return fmt.Errorf("current: %w", err)
	}
	rel, err := Operations(released)
	if err != nil {
		return fmt.Errorf("released: %w", err)
	}
	have := map[string]map[string]bool{}
	for _, o := range cur {
		have[o.String()] = map[string]bool{}
		for _, s := range o.Success {
			have[o.String()][s] = true
		}
	}
	var missing []string
	for _, o := range rel {
		got, ok := have[o.String()]
		if !ok {
			missing = append(missing, o.String())
			continue
		}
		for _, s := range o.Success {
			if !got[s] {
				missing = append(missing, o.String()+" "+s)
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("dropped from the released major: %s", strings.Join(missing, ", "))
	}
	return nil
}
