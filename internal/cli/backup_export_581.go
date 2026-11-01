package cli

// The owner export (GitHub #581 D5, enterprise-packaging-design.md amendment
// 2026-10-29, the D5 correction). `vornikctl backup --export` is the
// customer's data and not EaseIT's means to run it: no secrets/, the config
// passed through the config dump's redaction, and every table the census
// classes SecretMaterial left out of the dump (schema kept, rows dropped).
// An export restores to an instance whose secrets, keys and connectors are
// set up again; --restorable is the disaster-recovery backup.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// exportExcludeTableData is pg_dump's arguments for the SecretMaterial
// tables.
func exportExcludeTableData() []string {
	var args []string
	for _, t := range persistence.SecretMaterialTables() {
		args = append(args, "--exclude-table-data=public."+t)
	}
	return args
}

// redactYAMLFile writes src to dst, 0600, through secrets.RedactConfig.
func redactYAMLFile(src, dst string) error {
	raw, err := os.ReadFile(src) // #nosec G304 -- the deployment's own config
	if err != nil {
		return err
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%s: not YAML, so it cannot be redacted: %w", src, err)
	}
	// yaml.v3 decodes a mapping with any non-string key into map[any]any,
	// which RedactConfig does not walk, so a secret below one would pass
	// (commit security review of cef90952f). Keys are made strings first,
	// and a value of a type the walk does not know refuses the file.
	norm, err := stringKeyed(doc)
	if err != nil {
		return fmt.Errorf("%s: %w; not exported", src, err)
	}
	redacted, err := redactValues(secrets.RedactConfig(norm))
	if err != nil {
		return err
	}
	out, err := yaml.Marshal(redacted)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, out, 0o600)
}

// redactYAMLTree copies src's YAML files to dst redacted, and leaves every
// other file out: a file the redaction cannot read is not exported.
func redactYAMLTree(src, dst string) (int, error) {
	n := 0
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		n++
		return redactYAMLFile(path, filepath.Join(dst, rel))
	})
	return n, err
}

// stageExportConfig writes the export's redacted config.yaml and configs/
// into staging, and says what was left out.
func stageExportConfig(configPath, staging string) error {
	if err := redactYAMLFile(configPath, filepath.Join(staging, "config.yaml")); err != nil {
		return fmt.Errorf("redact config: %w", err)
	}
	n := 0
	if dir := resolveConfigsDir(configPath); dir != "" {
		var err error
		if n, err = redactYAMLTree(dir, filepath.Join(staging, "configs")); err != nil {
			return fmt.Errorf("redact configs dir: %w", err)
		}
	}
	secret := len(persistence.SecretMaterialTables())
	fmt.Printf("✔ config redacted (config.yaml and %d file(s) under configs/)\n", n)
	fmt.Printf("✔ %d secret-material table(s) left out of the dump, %d exported\n", secret, len(persistence.TableCensus)-secret)
	return nil
}

// stringKeyed returns v with every mapping as map[string]any, recursively,
// so RedactConfig walks all of it. A scalar key becomes its text; a key that
// is itself a mapping or a sequence, or a value of a type other than the
// ones YAML decodes to, is an error.
func stringKeyed(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, inner := range t {
			n, err := stringKeyed(inner)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, inner := range t {
			switch k.(type) {
			case string, int, int64, uint64, float64, bool, nil:
			default:
				return nil, fmt.Errorf("a mapping key of type %T cannot be redacted", k)
			}
			n, err := stringKeyed(inner)
			if err != nil {
				return nil, err
			}
			out[fmt.Sprint(k)] = n
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, inner := range t {
			n, err := stringKeyed(inner)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	case string, int, int64, uint64, float64, bool, nil, time.Time:
		return v, nil
	default:
		return nil, fmt.Errorf("a value of type %T cannot be redacted", v)
	}
}

// exportRedactionMark replaces a value the export removes.
const exportRedactionMark = "[REDACTED]"

// redactValues checks every string leaf by value, after RedactConfig's pass
// by key name (commit security review of bab44b9b2): a URL loses its userinfo
// and its query, and every match of the secrets detector's strong patterns
// is redacted. The entropy heuristic is not used: it fires on prose.
func redactValues(v any) (any, error) {
	det, err := secrets.NewMultiDetector(secrets.Config{Patterns: secrets.StrongPatterns(secrets.DefaultPatterns())})
	if err != nil {
		return nil, err
	}
	var walk func(any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case map[string]any:
			for k, inner := range t {
				t[k] = walk(inner)
			}
			return t
		case []any:
			for i, inner := range t {
				t[i] = walk(inner)
			}
			return t
		case string:
			return redactStringValue(det, t)
		default:
			return v
		}
	}
	return walk(v), nil
}

func redactStringValue(det secrets.Detector, s string) string {
	// A URL's userinfo anywhere in the value (commit security review of
	// 41e07408b), then, for a value that is a whole URL, its query too.
	s = stripURLUserinfo(s)
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		if u.User != nil {
			u.User = url.User(exportRedactionMark)
		}
		if u.RawQuery != "" {
			u.RawQuery = exportRedactionMark
		}
		s = u.String()
	}
	if found := det.Scan([]byte(s)); len(found) > 0 {
		s = string(secrets.Redact([]byte(s), found))
	}
	return s
}

// credentialFileNames are files a workspace export leaves out: git's stored
// credentials, netrc and dotenv files, and the clients' credential stores.
// It is a first pass only; the guarantee is scrubTextFile on every text file.
func isCredentialFile(name string) bool {
	switch name {
	case ".git-credentials", ".netrc", "_netrc", ".env", ".pgpass", ".npmrc", ".pypirc", ".dockercfg":
		return true
	}
	return strings.HasPrefix(name, ".env.")
}

// anyURLUserinfo is a URL's userinfo anywhere in text: a scheme, "://", and
// everything up to the last "@" before the host.
var anyURLUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s"'<>@]*(?:@[^/\s"'<>@]*)*@`)

// stripURLUserinfo removes every URL userinfo from s.
func stripURLUserinfo(s string) string { return anyURLUserinfo.ReplaceAllString(s, "${1}") }

// gitExtraHeader is an http.*.extraheader line, where actions/checkout keeps
// its token.
var gitExtraHeader = regexp.MustCompile(`(?i)^\s*extraheader\s*=`)

// gitCredentialSection is a credential section header in either form:
// [credential], [credential "<url>"] or [credential.<url>].
var gitCredentialSection = regexp.MustCompile(`(?i)^\s*\[credential([\s.][^\]]*)?\]`)

// gitSectionHeader is any section header.
var gitSectionHeader = regexp.MustCompile(`^\s*\[`)

// scrubbedTextLimit bounds the files scrubTextFile rewrites; a larger file is
// not a config or a note and is copied as it is.
const scrubbedTextLimit = 2 << 20

// scrubText drops git credential sections and extraheader lines, strips
// every URL userinfo, and redacts every strong-pattern match, line by line.
// It applies to any text: a credential section or extraheader line is git
// config syntax, and dropping one from another file loses nothing.
func scrubText(det secrets.Detector, raw []byte) []byte {
	var out []string
	inCredential := false
	for _, line := range strings.Split(string(raw), "\n") {
		if gitSectionHeader.MatchString(line) {
			inCredential = gitCredentialSection.MatchString(line)
		}
		if inCredential || gitExtraHeader.MatchString(line) {
			continue
		}
		line = stripURLUserinfo(line)
		if found := det.Scan([]byte(line)); len(found) > 0 {
			line = string(secrets.Redact([]byte(line), found))
		}
		out = append(out, line)
	}
	return []byte(strings.Join(out, "\n"))
}

// isText reports whether b looks like text: no NUL in its first 8 KiB.
func isText(b []byte) bool {
	head := b
	if len(head) > 8<<10 {
		head = head[:8<<10]
	}
	return !bytes.Contains(head, []byte{0})
}

// scrubWorkspaceCredentials, on the export's own copy of the workspaces
// (commit security reviews of bab44b9b2 and 41e07408b), removes credential
// files and scrubs every text file up to scrubbedTextLimit with scrubText.
// Binary files (git objects among them: the owner's committed history) are
// copied as they are. It returns how many files it removed and rewrote.
func scrubWorkspaceCredentials(dir string) (removed, rewritten int, err error) {
	det, err := secrets.NewMultiDetector(secrets.Config{Patterns: secrets.StrongPatterns(secrets.DefaultPatterns())})
	if err != nil {
		return 0, 0, err
	}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if isCredentialFile(d.Name()) {
			removed++
			return os.Remove(path)
		}
		info, err := d.Info()
		if err != nil || info.Size() > scrubbedTextLimit {
			return err
		}
		raw, err := os.ReadFile(path) // #nosec G304 -- walked under the export's staging copy
		if err != nil {
			return err
		}
		if !isText(raw) {
			return nil
		}
		out := scrubText(det, raw)
		if bytes.Equal(out, raw) {
			return nil
		}
		rewritten++
		return os.WriteFile(path, out, 0o600)
	})
	return removed, rewritten, err
}

// scrubDumpFile scrubs the export's database dump by value, line by line, in
// place (commit security review of 41e07408b): every URL's userinfo, and
// every strong-pattern match. The census leaves the rows of SecretMaterial
// tables out; this covers what an owner-data row holds (a task's result, a
// workflow definition) wherever it is. A replacement holds no tab or newline,
// so the COPY format survives. It returns how many lines it changed.
func scrubDumpFile(path string) (int, error) {
	det, err := secrets.NewMultiDetector(secrets.Config{Patterns: secrets.StrongPatterns(secrets.DefaultPatterns())})
	if err != nil {
		return 0, err
	}
	in, err := os.Open(path) // #nosec G304 -- the export's own staging file
	if err != nil {
		return 0, err
	}
	defer func() { _ = in.Close() }()
	tmp := path + ".scrubbed"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- beside the staging file
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.Remove(tmp) }()
	r := bufio.NewReaderSize(in, 1<<20)
	w := bufio.NewWriterSize(out, 1<<20)
	changed := 0
	for {
		line, rerr := r.ReadString('\n')
		if line != "" {
			scrubbed := stripURLUserinfo(line)
			if found := det.Scan([]byte(scrubbed)); len(found) > 0 {
				scrubbed = string(secrets.Redact([]byte(scrubbed), found))
			}
			if scrubbed != line {
				changed++
			}
			if _, err := w.WriteString(scrubbed); err != nil {
				_ = out.Close()
				return 0, err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = out.Close()
			return 0, rerr
		}
	}
	if err := w.Flush(); err != nil {
		_ = out.Close()
		return 0, err
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	return changed, os.Rename(tmp, path)
}
