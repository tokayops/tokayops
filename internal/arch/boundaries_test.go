// Package arch holds the boundary rules of the tree as a test.
//
// The rules themselves, and the reasoning behind them, are in
// tokay-docs/domains.md. What is here is the half that cannot drift: a document
// describing boundaries is checked by whoever remembers to read it, and this
// week alone two such statements in that document were wrong within a day of
// being written. A rule worth having is a rule that fails a build.
//
// Both tests read source, not a database: they parse the tree and answer from
// the syntax. Adding an exception is deliberate and visible in the diff, which
// is the point - the list below is the honest inventory of what the tree does
// not yet obey.
package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// storePackage is the persistence layer: the one package that may hold SQL and
// know every table.
const storePackage = "internal/store"

// mayReachStore lists the packages allowed to depend on the persistence layer,
// with the reason each is still on the list. Removing an entry is the goal;
// adding one is a decision that belongs in domains.md as well as here.
var mayReachStore = map[string]string{
	"internal/store":      "it is the persistence layer",
	"internal/api":        "the HTTP layer still writes to every domain directly - the main known violation",
	"internal/dispatcher": "parts of four things in one package; narrowed together with the job engine contract",
	"internal/testutil":   "test scaffolding: it hands tests a real store",
	"cmd/tokayops":        "the wiring that constructs the store and passes it to everyone else",
}

// TestDomainPackagesDoNotReachTheStore is rule 2 of domains.md: a domain
// package declares what it needs as its own interface, and the concrete store
// is passed in at wiring time.
//
// Reachability, not direct imports: a package that imports a package that
// imports the store depends on the store just as surely, and that is exactly
// how the metrics read model kept four packages coupled until it moved.
func TestDomainPackagesDoNotReachTheStore(t *testing.T) {
	root := moduleRoot(t)
	graph := importGraph(t, root)

	for pkg := range graph {
		if _, allowed := mayReachStore[pkg]; allowed {
			continue
		}
		if path := reaches(graph, pkg, storePackage); path != nil {
			t.Errorf("%s depends on %s\n    through: %s\n"+
				"    declare what this package needs from the store as its own interface,\n"+
				"    or add it to mayReachStore with the reason, and say so in domains.md",
				pkg, storePackage, strings.Join(path, " -> "))
		}
	}
}

// writeStatements match statements that change rows, by shape rather than by
// keyword.
//
// The keyword alone finds "failed to update the timeline" and "an update cannot
// end an override in the past"; a check that cries about prose is a check
// somebody switches off. A relation after INTO or FROM, and a SET after the
// relation, is what an actual statement looks like.
//
// Two limits, stated rather than discovered later. A statement split across
// concatenated literals - "UPDATE jobs " + "SET ..." - is not found, because
// each literal is examined on its own. And the list is INSERT, UPDATE, DELETE
// and TRUNCATE; MERGE and COPY would pass, and nothing here uses them.
//
// TRUNCATE has to spell out TABLE, or "failed to truncate tables" is a finding.
var writeStatements = []*regexp.Regexp{
	regexp.MustCompile(`(?is)\bINSERT\s+INTO\s+[a-z_"]`),
	regexp.MustCompile(`(?is)\bUPDATE\s+[a-z_."]+\s+SET\b`),
	regexp.MustCompile(`(?is)\bDELETE\s+FROM\s+[a-z_"]`),
	regexp.MustCompile(`(?is)\bTRUNCATE\s+TABLE\s+[a-z_"%]`),
}

// mayWriteSQL lists the packages allowed to change rows, with the reason. It is
// separate from mayReachStore because the two rules are different: one is about
// who may hold a connection, the other about who may issue a statement.
var mayWriteSQL = map[string]string{
	"internal/store":    "it is the persistence layer",
	"internal/testutil": "test scaffolding: it truncates the tables between tests",
}

// TestWriteSQLStaysInPersistence proves one thing, and it is narrower than
// rule 1 of domains.md: physical write SQL is centralised in the persistence
// package and the test scaffolding allowlisted below.
//
// It says nothing about who AUTHORISED a mutation. The schedule repository
// deleting team_members is a breach of rule 1 and passes here, because that
// statement lives in the store like every other. A table-to-owner guard becomes
// possible when the first SQL moves out of this package, and not before.
//
// It reads string literals rather than file text, so a comment naming FOR
// UPDATE is not a finding - two of those exist in the tree.
//
// SELECT is deliberately absent: reading across domains is cheap and often
// legitimate, and it is writes that break invariants.
func TestWriteSQLStaysInPersistence(t *testing.T) {
	root := moduleRoot(t)

	for _, dir := range []string{"internal", "cmd"} {
		walkGoFiles(t, filepath.Join(root, dir), func(path string, file *ast.File, fset *token.FileSet) {
			pkg := packagePath(root, path)
			if _, allowed := mayWriteSQL[pkg]; allowed {
				return
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				if writesRows(lit.Value) {
					t.Errorf("%s: SQL that changes rows outside %s\n    %s\n"+
						"    the persistence layer issues statements; ask the owning package instead,\n"+
						"    or add this package to mayWriteSQL with the reason",
						fset.Position(lit.Pos()), storePackage, strings.TrimSpace(lit.Value))
					return false
				}
				return true
			})
		})
	}
}

// writesRows reports whether a string literal holds a statement that changes
// rows.
//
// It matches the literal's VALUE, not its source text, and that is not a
// detail: in "UPDATE jobs\nSET status = $1" the newline is two source
// characters - a backslash and an n - which no whitespace class matches. A
// check reading the source would pass that statement, and every multi-line
// query written as an interpreted string with it.
func writesRows(literal string) bool {
	value, err := strconv.Unquote(literal)
	if err != nil {
		// Not a form this can read; fall back to the source rather than
		// letting an unreadable literal be an automatic pass.
		value = literal
	}
	for _, write := range writeStatements {
		if write.MatchString(value) {
			return true
		}
	}
	return false
}

// TestWriteStatementMatcher pins what writesRows answers, in both literal
// forms and for the prose that keeps looking like SQL.
func TestWriteStatementMatcher(t *testing.T) {
	cases := []struct {
		name    string
		literal string
		want    bool
	}{
		{"raw literal", "`UPDATE jobs SET status = $1`", true},
		{"raw literal across lines", "`UPDATE jobs\n\t\tSET status = $1`", true},
		{"interpreted literal with an escaped newline", `"UPDATE jobs
SET status = $1"`, true},
		{"insert", `"INSERT INTO jobs (id) VALUES ($1)"`, true},
		{"delete", "`DELETE FROM job_steps WHERE job_id = $1`", true},
		{"truncate", `"TRUNCATE TABLE jobs RESTART IDENTITY"`, true},

		{"prose about updating", `"failed to update the timeline: %v"`, false},
		{"prose about truncating", `"Failed to truncate tables: %v"`, false},
		{"prose about an update", `"an update cannot end an override in the past"`, false},
		{"a select", "`SELECT id FROM jobs WHERE status = $1`", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := writesRows(tc.literal); got != tc.want {
				t.Errorf("writesRows(%s) = %v, want %v", tc.literal, got, tc.want)
			}
		})
	}
}

// importGraph maps every package in the tree to the packages of this module it
// imports. Test files are left out: a test may reach for a real store, and
// saying so in a _test.go file is not what these rules are about.
func importGraph(t *testing.T, root string) map[string][]string {
	t.Helper()

	module := modulePath(t, root)
	graph := map[string][]string{}

	for _, dir := range []string{"internal", "cmd"} {
		walkGoFiles(t, filepath.Join(root, dir), func(path string, file *ast.File, _ *token.FileSet) {
			pkg := packagePath(root, path)
			if _, seen := graph[pkg]; !seen {
				graph[pkg] = nil
			}
			for _, spec := range file.Imports {
				imported, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					continue
				}
				if !strings.HasPrefix(imported, module+"/") {
					continue
				}
				graph[pkg] = append(graph[pkg], strings.TrimPrefix(imported, module+"/"))
			}
		})
	}
	return graph
}

// reaches returns the path from start to target, or nil. The path is what makes
// a failure actionable: "engine depends on store" is a puzzle, "engine ->
// metrics -> store" is an instruction.
func reaches(graph map[string][]string, start, target string) []string {
	type step struct {
		pkg  string
		path []string
	}
	seen := map[string]bool{start: true}
	queue := []step{{pkg: start, path: []string{start}}}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range graph[current.pkg] {
			if seen[next] {
				continue
			}
			path := append(append([]string{}, current.path...), next)
			if next == target {
				return path
			}
			seen[next] = true
			queue = append(queue, step{pkg: next, path: path})
		}
	}
	return nil
}

func walkGoFiles(t *testing.T, dir string, fn func(path string, file *ast.File, fset *token.FileSet)) {
	t.Helper()
	fset := token.NewFileSet()

	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		fn(path, parsed, fset)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

func packagePath(root, file string) string {
	rel, err := filepath.Rel(root, filepath.Dir(file))
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test")
		}
		dir = parent
	}
}

func modulePath(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if after, found := strings.CutPrefix(strings.TrimSpace(line), "module "); found {
			return strings.TrimSpace(after)
		}
	}
	t.Fatal("go.mod has no module line")
	return ""
}
