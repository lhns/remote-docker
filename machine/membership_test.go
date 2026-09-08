package machine

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This module imports nothing from this repository, which is what makes it a
// module rather than a package (ADR 0021). A test rather than a line in a
// document, because an import of `client/internal/config` compiles and passes
// while quietly putting docker/cli back in the graph the boundary exists for.
//
// Reading the source rather than running `go list -deps` covers every GOOS at
// once: `wsl_windows.go` and `hyperv_windows.go` are build-tagged, so a
// dependency added in one of them is invisible to a `go list` run on Linux.
// Direct imports are the whole test, since nothing outside this repository can
// import back into it.
func TestImportsNothingFromThisRepository(t *testing.T) {
	const repo = "github.com/lhns/remote-docker/"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", entry.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(path, repo) {
				t.Errorf("%s imports %s; this module must reach nothing in this repository", entry.Name(), path)
			}
		}
	}
}
