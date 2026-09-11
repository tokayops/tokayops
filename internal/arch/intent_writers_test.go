package arch

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var intentInsert = regexp.MustCompile(`INSERT\s+INTO\s+outbound_intents\b`)

// TestTheIntentTableHasOneWriter names the one statement that makes a
// commitment and the two doors it is behind: an admission, which makes every
// commitment a producer proposes, and the start of the first version with
// satellites, which gives them to the cards of the previous one. A third
// writer would be a commitment made outside both, with a journal, a claim
// count and a parent nobody checked.
func TestTheIntentTableHasOneWriter(t *testing.T) {
	root := moduleRoot(t)
	const writer = "insertCommitmentRowTx"
	wantCallers := []string{"admitSatellitesTx", "insertCommitmentsTx"}

	inserts := map[string]string{}
	callers := map[string]string{}
	for _, dir := range []string{"internal", "cmd"} {
		walkGoFiles(t, filepath.Join(root, dir), func(path string, file *ast.File, fset *token.FileSet) {
			if strings.HasSuffix(path, "_test.go") {
				return
			}
			enclosing := ""
			ast.Inspect(file, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.FuncDecl:
					enclosing = x.Name.Name
				case *ast.BasicLit:
					if x.Kind == token.STRING && intentInsert.MatchString(x.Value) {
						inserts[enclosing] = fset.Position(x.Pos()).String()
					}
				case *ast.CallExpr:
					if name, ok := x.Fun.(*ast.Ident); ok && name.Name == writer {
						callers[enclosing] = fset.Position(x.Pos()).String()
					}
				}
				return true
			})
		})
	}

	if got := names(inserts); strings.Join(got, ",") != writer {
		t.Fatalf("commitments are inserted by %v, and the one writer is %s\n%v", got, writer, inserts)
	}
	if got := names(callers); strings.Join(got, ",") != strings.Join(wantCallers, ",") {
		t.Fatalf("%s is called from %v, and its two doors are %v\n%v", writer, got, wantCallers, callers)
	}
}

func names(found map[string]string) []string {
	out := make([]string, 0, len(found))
	for name := range found {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
