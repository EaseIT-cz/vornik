package hermes

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
)

// SentinelFindings lists every place a sentinel appears: the given texts
// (by label) and every regular file under dirs. Unreadable files are
// reported too, so an unscanned file cannot pass as clean (CLAUDE.md §4).
func SentinelFindings(sentinel string, texts map[string]string, dirs ...string) []string {
	var out []string
	needle := []byte(sentinel)
	for label, s := range texts {
		if bytes.Contains([]byte(s), needle) {
			out = append(out, "text:"+label)
		}
	}
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				out = append(out, "unreadable:"+path)
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				out = append(out, "unreadable:"+path)
				return nil
			}
			if bytes.Contains(b, needle) {
				out = append(out, "file:"+path)
			}
			return nil
		})
	}
	return out
}
