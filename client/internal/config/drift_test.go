package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// notDocumented is every variable the code names and README's tables do not,
// each with the reason. An entry here is a decision; a variable that is
// missing from both is drift.
var notDocumented = map[string]string{
	"WORKSPACE_PRUNE_INTERVAL": "a test hook: how often the age rules run (ADR 0056)",
	"WORKSPACE_PRUNE_FLOOR":    "a test hook: how long an image is safe from the usage rule (ADR 0056)",
}

// The environment variables a person can set are documented in README's two
// settings tables, and nothing else keeps them honest: a variable added to the
// code and not to the table is invisible until somebody reads the source. This
// reads both sides.
func TestEveryVariableTheCodeReadsIsInReadmesTables(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Skipf("no README to compare against: %v", err)
	}
	documented := map[string]bool{}
	for _, line := range strings.Split(string(readme), "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		for _, m := range regexp.MustCompile("`((?:REMOTE_DOCKER|WORKSPACE)_[A-Z0-9_]+)`").FindAllStringSubmatch(line, -1) {
			documented[m[1]] = true
		}
	}

	for _, tc := range []struct {
		prefix string
		dirs   []string
	}{
		{"REMOTE_DOCKER_", []string{"client", "core-client", "core"}},
		{"WORKSPACE_", []string{"agent", "core-agent", "charts", "deploy"}},
	} {
		names := regexp.MustCompile(tc.prefix + "[A-Z0-9_]+")
		used := map[string]string{} // name -> first file
		for _, dir := range tc.dirs {
			err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				// Tests name variables of their own making.
				if strings.HasSuffix(path, "_test.go") {
					return nil
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				for _, name := range names.FindAllString(string(data), -1) {
					if _, ok := used[name]; !ok {
						rel, _ := filepath.Rel(root, path)
						used[name] = filepath.ToSlash(rel)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		if len(used) == 0 {
			t.Errorf("found no %s* names under %v: the scan is looking in the wrong place", tc.prefix, tc.dirs)
		}
		for name, file := range used {
			if documented[name] {
				continue
			}
			if reason, ok := notDocumented[name]; ok {
				if reason == "" {
					t.Errorf("%s is excluded without a reason", name)
				}
				continue
			}
			t.Errorf("%s (used in %s) is in none of README's settings tables; add a row, or list it in notDocumented with a reason", name, file)
		}
	}
}
