package conceptgate

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// registryClean refuses a registry directory, rel below tree, with tracked or untracked changes.
func registryClean(ctx context.Context, tree, rel string) error {
	status, err := exec.CommandContext(ctx, "git", "-C", tree, "status", "--porcelain", "--untracked-files=all", "--", rel).Output()
	if err != nil {
		return fmt.Errorf("check registry changes: %w", err)
	}
	if len(status) != 0 {
		return fmt.Errorf("the registry has local changes")
	}
	return nil
}

// snapshotRegistry extracts the registry directory rel of tree, as committed at commit, into a new directory below
// base, and gives the extracted registry directory and the cleanup that removes the snapshot. The gate and the
// recheck read only this immutable snapshot, never the tree's mutable working directory.
func snapshotRegistry(ctx context.Context, tree, commit, rel, base string) (string, func(), error) {
	root, err := os.MkdirTemp(base, "registry-snapshot-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	cmd := exec.CommandContext(ctx, "git", "-C", tree, "archive", "--format=tar", commit, "--", rel)
	out, err := cmd.StdoutPipe()
	if err != nil {
		cleanup()
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		cleanup()
		return "", nil, err
	}
	fail := func(err error) (string, func(), error) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		cleanup()
		return "", nil, err
	}
	if err := extractRegistryArchive(out, root, rel); err != nil {
		return fail(err)
	}
	if _, err := io.Copy(io.Discard, out); err != nil {
		return fail(err)
	}
	if err := cmd.Wait(); err != nil {
		cleanup()
		return "", nil, err
	}
	registry := filepath.Join(root, rel)
	if info, err := os.Stat(registry); err != nil || !info.IsDir() {
		cleanup()
		if err != nil {
			return "", nil, err
		}
		return "", nil, fmt.Errorf("the index commit has no registry directory %s", rel)
	}
	return registry, cleanup, nil
}

// extractRegistryArchive writes the directories and regular files of a `git archive` tar of rel below root. An
// entry whose name is not local (absolute, or leaving the root through `..`) or lies outside rel is refused before
// anything is written for it, and links and other entry types are refused, so no entry writes outside root.
func extractRegistryArchive(archive io.Reader, root, rel string) error {
	prefix := filepath.ToSlash(filepath.Clean(rel))
	reader := tar.NewReader(archive)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeXGlobalHeader || header.Typeflag == tar.TypeXHeader {
			continue
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if !filepath.IsLocal(name) {
			return fmt.Errorf("git archive returned unsafe registry path: %s", header.Name)
		}
		slash := filepath.ToSlash(name)
		if header.Typeflag == tar.TypeDir && strings.HasPrefix(prefix, slash+"/") {
			// `git archive` also lists the directories that lead to a nested registry.
			if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
				return err
			}
			continue
		}
		if slash != prefix && !strings.HasPrefix(slash, prefix+"/") {
			return fmt.Errorf("git archive returned path outside registry: %s", header.Name)
		}
		target := filepath.Join(root, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, reader)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("git archive returned unsupported registry entry %s", header.Name)
		}
	}
}
