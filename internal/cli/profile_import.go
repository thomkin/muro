package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/thomkin/muro/internal/config"
)

var (
	profileImportRefFlag       string
	profileImportOverwriteFlag bool
)

// profileImportCmd shallow-clones a git repository into a throwaway temp
// dir and imports whatever profile BUNDLES it finds there — a bundle is a
// subdirectory containing a profile.json (config.ProfileBundleDir's own
// on-disk shape), plus whatever else sits alongside it (AGENT.md, other
// docs) — under profiles/ (or the repo root, if there's no profiles/
// directory). Nothing in the repo is ever read as code or executed at any
// point during import, only copied as data; the only file muro actually
// parses is each bundle's profile.json.
var profileImportCmd = &cobra.Command{
	Use:   "import <git-url>",
	Short: "Import profile bundles from a git repository (shallow clone, config + docs only)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		url := args[0]

		tmpDir, err := os.MkdirTemp("", "muro-profile-import-*")
		if err != nil {
			return fmt.Errorf("create temp dir: %w", err)
		}
		defer os.RemoveAll(tmpDir)

		cloneArgs := []string{"clone", "--depth", "1", "--quiet"}
		if profileImportRefFlag != "" {
			cloneArgs = append(cloneArgs, "--branch", profileImportRefFlag)
		}
		cloneArgs = append(cloneArgs, url, tmpDir)

		gitCmd := exec.Command("git", cloneArgs...)
		var stderr strings.Builder
		gitCmd.Stderr = &stderr
		if err := gitCmd.Run(); err != nil {
			return fmt.Errorf("git clone failed: %w: %s", err, strings.TrimSpace(stderr.String()))
		}

		searchDir := filepath.Join(tmpDir, "profiles")
		if info, statErr := os.Stat(searchDir); statErr != nil || !info.IsDir() {
			searchDir = tmpDir
		}

		entries, err := os.ReadDir(searchDir)
		if err != nil {
			return fmt.Errorf("read cloned repo: %w", err)
		}

		var imported, skipped []string
		for _, e := range entries {
			if !e.IsDir() {
				continue // a profile bundle is always a directory — a loose file here is never one
			}
			bundleDir := filepath.Join(searchDir, e.Name())
			name, skipReason := importOneBundle(bundleDir, profileImportOverwriteFlag)
			if skipReason != "" {
				skipped = append(skipped, fmt.Sprintf("%s (%s)", e.Name(), skipReason))
				continue
			}
			imported = append(imported, name)
		}

		for _, name := range imported {
			fmt.Printf("imported %q\n", name)
		}
		for _, msg := range skipped {
			fmt.Printf("skipped %s\n", msg)
		}
		if len(imported) == 0 {
			return fmt.Errorf("no profile bundles found to import (looked in %s)", searchDir)
		}
		fmt.Printf("\n%d imported, %d skipped. These came from an external source — review before running (`muro profile show <name>`).\n", len(imported), len(skipped))
		return nil
	},
}

// importOneBundle reads, validates, and copies a single candidate profile
// bundle directory (must contain profile.json) into its permanent local
// location (config.ProfileBundleDir(p.Name)) — the whole directory,
// including any docs alongside profile.json, not just the JSON itself.
// Returns the imported name on success, or a human-readable skip reason
// (never an error — one bad bundle must not abort the rest of the import).
func importOneBundle(bundleDir string, overwrite bool) (name, skipReason string) {
	jsonPath := filepath.Join(bundleDir, "profile.json")
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return "", "no profile.json in this directory"
	}
	var p config.Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return "", fmt.Sprintf("profile.json is not valid: %v", err)
	}
	if p.Name == "" {
		return "", "profile.json is missing its \"name\" field"
	}
	if err := config.ValidSandboxName("profile name", p.Name); err != nil {
		return "", err.Error()
	}
	if !overwrite {
		if _, err := config.LoadProfileRaw(p.Name); err == nil {
			return "", fmt.Sprintf("profile %q already exists locally — use --overwrite to replace it", p.Name)
		}
	}
	if err := config.ValidateProfile(&p); err != nil {
		return "", err.Error()
	}

	destDir, err := config.ProfileBundleDir(p.Name)
	if err != nil {
		return "", err.Error()
	}
	if overwrite {
		if err := os.RemoveAll(destDir); err != nil {
			return "", fmt.Sprintf("clear existing bundle: %v", err)
		}
	}
	if err := copyDir(bundleDir, destDir); err != nil {
		return "", fmt.Sprintf("copy bundle: %v", err)
	}
	// Re-save profile.json through SaveProfile (not left as the raw copy)
	// so it goes through the same atomic-write path and marshaling
	// normalization as any other profile write.
	if err := config.SaveProfile(&p); err != nil {
		return "", fmt.Sprintf("save failed: %v", err)
	}
	return p.Name, ""
}

// copyDir recursively copies src's contents into dst, creating dst (and
// any subdirectories) as needed. A bundle's docs (AGENT.md, etc.) have to
// travel to the local, permanent bundle directory alongside profile.json —
// not just the one file muro actually parses.
func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(srcPath, dstPath); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func init() {
	profileImportCmd.Flags().StringVar(&profileImportRefFlag, "ref", "", "branch or tag to clone (default: the repo's default branch)")
	profileImportCmd.Flags().BoolVar(&profileImportOverwriteFlag, "overwrite", false, "replace an existing local profile with the same name")
	profileCmd.AddCommand(profileImportCmd)
}
