package cli

// Restorable backups (GitHub #581 D4, enterprise-packaging-design.md
// amendment 2026-10-29). `vornikctl backup --restorable` adds the secrets/
// directory beside the config file (the secret store's store.key and the env
// files) so the sealed secrets open on a replacement VM. Such an archive is
// secret-equivalent: every backup archive is now written 0600, and restore
// puts secrets/ back only into the directory provision created, with that
// directory's owner.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// secretsDirFor is the secrets/ directory beside the config file, where the
// daemon reads it (service.onboardingSecretsDir).
func secretsDirFor(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "secrets")
}

// stageSecrets copies the regular files of secretsDir into staging/secrets,
// directories 0700 and files 0600 whatever their source modes. It returns how
// many files it copied. A missing secretsDir is an error: --restorable was
// asked for, and an archive without the store key would not restore.
//
// secretsDir is the daemon user's and this runs as root, so every read goes
// through an os.Root confined to it: a link the daemon user plants cannot
// make root read a file outside (commit security review of 54502c9b2).
func stageSecrets(secretsDir, staging string) (int, error) {
	root, err := openSecretsRoot(secretsDir)
	if err != nil {
		return 0, fmt.Errorf("--restorable: %w", err)
	}
	defer func() { _ = root.Close() }()
	dst := filepath.Join(staging, "secrets")
	n := 0
	err = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case d.Type().IsRegular():
			body, err := fs.ReadFile(root.FS(), rel)
			if err != nil {
				return err
			}
			n++
			return os.WriteFile(target, body, 0o600)
		default:
			return nil // never a link: the extractor drops them too
		}
	})
	return n, err
}

// openSecretsRoot opens dir as an os.Root after checking that dir itself is
// a real directory, not a link to one.
func openSecretsRoot(dir string) (*os.Root, error) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory (absent, or a link)", dir)
	}
	return os.OpenRoot(dir)
}

// createPrivateFile creates (or truncates) path at mode 0600 before anything
// is written to it, so an archive is never readable by others, not even for
// the moment before a chmod.
func createPrivateFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- the operator names the archive
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil { // an existing file keeps its mode through O_TRUNC
		_ = f.Close()
		return err
	}
	return f.Close()
}

// restoreSecrets puts an archive's secrets/ back into secretsDir. The
// directory must exist: provision creates it with the daemon user as owner,
// and every file restored takes that owner, mode 0600, written to a
// temporary beside it and renamed, so a reader never sees half a key.
//
// Restore runs as root into a directory the daemon user owns, so every write
// goes through an os.Root confined to secretsDir, and a directory that is a
// link is refused: a link the daemon user plants cannot carry a root write,
// or a chown, outside it (commit security review of 54502c9b2).
func restoreSecrets(staged, secretsDir string) (int, error) {
	info, err := os.Lstat(secretsDir)
	if err != nil || !info.IsDir() {
		return 0, fmt.Errorf("%s is not a directory (absent, or a link): run provision first, which creates it with its owner, then restore", secretsDir)
	}
	uid, gid := -1, -1
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		uid, gid = int(st.Uid), int(st.Gid)
	}
	root, err := os.OpenRoot(secretsDir)
	if err != nil {
		return 0, err
	}
	defer func() { _ = root.Close() }()
	n := 0
	err = filepath.WalkDir(staged, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(staged, path)
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if rel == "." {
				return nil
			}
			return restoreDir(root, rel, uid, gid)
		case d.Type().IsRegular():
			body, err := os.ReadFile(path) // #nosec G304 -- walked under the extracted archive
			if err != nil {
				return err
			}
			if err := writePrivateFileAtomic(root, rel, body, uid, gid); err != nil {
				return err
			}
			n++
			return nil
		default:
			return nil
		}
	})
	return n, err
}

// restoreDir makes rel under root a real 0700 directory owned by uid:gid; an
// existing link there is refused, not followed.
func restoreDir(root *os.Root, rel string, uid, gid int) error {
	if err := root.Mkdir(rel, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	info, err := root.Lstat(rel)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("restore secrets: %s exists and is not a directory (a link?); refusing to write through it", rel)
	}
	return lchownIfRoot(root, rel, uid, gid)
}

func writePrivateFileAtomic(root *os.Root, rel string, body []byte, uid, gid int) error {
	tmp := filepath.Join(filepath.Dir(rel), "."+filepath.Base(rel)+".restore-"+strconv.Itoa(os.Getpid()))
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(tmp) }()
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := lchownIfRoot(root, tmp, uid, gid); err != nil {
		return err
	}
	// Rename replaces whatever is at rel, a link included, and never
	// follows it.
	return root.Rename(tmp, rel)
}

// lchownIfRoot gives rel under root the owner when this process can: as root
// (the managed restore runs as root). As anyone else the files are already
// the caller's, which is the owner a self host's secrets/ has.
func lchownIfRoot(root *os.Root, rel string, uid, gid int) error {
	if os.Geteuid() != 0 || uid < 0 {
		return nil
	}
	if err := root.Lchown(rel, uid, gid); err != nil {
		return errors.Join(fmt.Errorf("chown %s", rel), err)
	}
	return nil
}
