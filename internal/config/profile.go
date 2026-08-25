package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mount is one directory (or file) bind-mounted into a sandbox at launch.
type Mount struct {
	Host        string `json:"host"`
	SandboxPath string `json:"sandbox_path"`
	Mode        string `json:"mode"` // "ro" | "rw"
}

// Tool is one host executable (or, with As == "*", a whole curated
// toolchain directory) exposed inside a sandbox's restricted PATH
// (DESIGN.md §10).
type Tool struct {
	Host string `json:"host"`
	As   string `json:"as"` // sandbox-visible name, or "*" for a whole directory
}

// Profile is a named, reusable sandbox configuration (SPEC.md §4/§7),
// stored as JSON at <ProfilesDir>/<name>.json.
type Profile struct {
	Name          string            `json:"name"`
	Agent         string            `json:"agent"`
	Mounts        []Mount           `json:"mounts"`
	Tools         []Tool            `json:"tools"`
	AllowURLs     []string          `json:"allow_urls"`
	DenyURLs      []string          `json:"deny_urls"`
	Env           map[string]string `json:"env"`
	RestartPolicy string            `json:"restart_policy"` // never|on-failure|always
}

func profilePath(name string) (string, error) {
	dir, err := ProfilesDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

// LoadProfile reads and parses <ProfilesDir>/<name>.json.
func LoadProfile(name string) (*Profile, error) {
	path, err := profilePath(name)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parsing profile %q: %w", name, err)
	}
	if p.RestartPolicy == "" {
		p.RestartPolicy = "never"
	}
	return &p, nil
}

// SaveProfile writes p to <ProfilesDir>/<p.Name>.json atomically: it
// writes to a temp file in the same directory and renames it into place,
// so a reader (or a crash mid-write) never observes a partially written
// profile.
func SaveProfile(p *Profile) error {
	if p.Name == "" {
		return fmt.Errorf("profile has no name")
	}

	dir, err := ProfilesDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".tmp-"+p.Name+"-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// Clean up the temp file if we return before the rename succeeds.
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	finalPath := filepath.Join(dir, p.Name+".json")
	return os.Rename(tmpPath, finalPath)
}

// ListProfiles returns the names of all profiles in ProfilesDir (without
// the .json extension). Returns an empty slice, not an error, if the
// directory doesn't exist yet.
func ListProfiles() ([]string, error) {
	dir, err := ProfilesDir()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	return names, nil
}
