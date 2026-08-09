package dropindex

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This file is the machine-checked form of TRD §17.3 and AC-21: operations/
// holds planners exclusively, and the absence of an executor here is the
// structural expression of §6.2. A reviewer is told to check it first; these
// tests check it on every run.

// sourceFiles returns the package's non-test .go files.
func sourceFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		t.Fatal("found no source files; the structural checks would pass vacuously")
	}
	return out
}

// AC-21: the planner depends on nothing that could execute anything. It may
// import the standard library, engine/protocol and engine/planner, and that is
// all. In particular it never imports a database driver, which is what keeps
// the whole tree unit-testable with no PostgreSQL reachable.
func TestPackageImportsNothingThatCanExecute(t *testing.T) {
	forbidden := map[string]string{
		"database/sql":        "a planner never opens a database handle (FR-PLAN-8)",
		"database/sql/driver": "a planner never speaks the driver protocol",
		"net":                 "a planner never opens a socket",
		"net/http":            "a planner never opens a socket",
		"os/exec":             "a planner never shells out",
		"github.com/atulsinha87/partitionctl/engine/executor": "a planner never links the executor",
	}
	allowedNonStdlib := map[string]bool{
		"github.com/atulsinha87/partitionctl/engine/protocol": true,
		"github.com/atulsinha87/partitionctl/engine/planner":  true,
	}

	fset := token.NewFileSet()
	for _, name := range sourceFiles(t) {
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: bad import %s", name, imp.Path.Value)
			}
			if why, bad := forbidden[path]; bad {
				t.Errorf("%s imports %q: %s", name, path, why)
			}
			if strings.Contains(path, ".") && !allowedNonStdlib[path] {
				t.Errorf("%s imports %q; this package depends only on the standard library, "+
					"engine/protocol and engine/planner", name, path)
			}
		}
	}
}

// TRD §17.3: there is no operations/*/executor/. The directory holds planner
// source and nothing else.
func TestPackageContainsNoExecutor(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("operations/drop-index contains a subdirectory %q; "+
				"operations/ holds planners exclusively (TRD §17.3, AC-21)", e.Name())
		}
		if strings.Contains(strings.ToLower(e.Name()), "exec") {
			t.Errorf("operations/drop-index contains %q", e.Name())
		}
	}
	for _, name := range sourceFiles(t) {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		// Method names that would mean this package runs statements. Comments
		// in this package never use them, so a substring match is enough and
		// is deliberately blunt.
		for _, needle := range []string{
			".Exec(", ".ExecContext(", ".Query(", ".QueryContext(", ".QueryRow(",
			".Begin(", ".BeginTx(", "sql.Open(",
		} {
			if strings.Contains(string(src), needle) {
				t.Errorf("%s contains %q; this package issues no statements (FR-PLAN-8, AC-21)", name, needle)
			}
		}
	}
}

// NFR-EXT-1 evidence in the small: adding an operation is one
// [planner.OperationPlanner] and nothing else. This package declares no
// interface of its own, because every seam it needs already exists on
// planner.Request.
func TestPackageDeclaresNoNewSeams(t *testing.T) {
	for _, name := range sourceFiles(t) {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "type ") && strings.Contains(trimmed, "interface") {
				t.Errorf("%s declares %q; this operation adds no second catalog interface and no "+
					"second specification (directive C.1)", name, trimmed)
			}
		}
	}
}

// The package doc is load-bearing: it is what tells the next reader that the
// absence of an executor is deliberate.
func TestPackageDocStatesTheConstraint(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(".", "doc.go"))
	if err != nil {
		t.Fatalf("read doc.go: %v", err)
	}
	for _, phrase := range []string{"AC-21", "planner", "no executor"} {
		if !strings.Contains(strings.ToLower(string(src)), strings.ToLower(phrase)) {
			t.Errorf("doc.go does not mention %q", phrase)
		}
	}
}
