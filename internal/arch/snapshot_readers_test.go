package arch

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestSnapshotV1HasTwoReaders names the only two places the previous render
// snapshot version is read: the admission state of a batch frozen before the
// upgrade, which a one-shot message renders as it was, and the start-up rebuild
// of the group snapshots, which reads a version 1 row once to write it as
// version 2. A third reader would be a path rendering old rows the rebuild was
// supposed to have removed - or writing new ones - and is refused here.
func TestSnapshotV1HasTwoReaders(t *testing.T) {
	root := moduleRoot(t)
	want := []string{"checkedSnapshot", "rebuildRenderSnapshots"}

	readers := map[string]string{}
	for _, dir := range []string{"internal", "cmd"} {
		walkGoFiles(t, filepath.Join(root, dir), func(path string, file *ast.File, fset *token.FileSet) {
			if strings.HasSuffix(path, "_test.go") ||
				strings.Contains(filepath.ToSlash(path), "/internal/outbound/keys/") {
				return
			}
			enclosing := ""
			ast.Inspect(file, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.FuncDecl:
					enclosing = x.Name.Name
				case *ast.SelectorExpr:
					if x.Sel.Name == "DecodeRenderSnapshotV1" {
						readers[enclosing] = fset.Position(x.Pos()).String()
					}
				}
				return true
			})
		})
	}

	got := make([]string, 0, len(readers))
	for name := range readers {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the version 1 snapshot is read by %v, and the two readers it has are %v\n%v",
			got, want, readers)
	}
}
