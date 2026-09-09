package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestACommentStaysWithTheDeclarationItNames. A comment that opens with the
// name of a declaration is that declaration's documentation, and it has to
// stand directly above it. An insertion that lands between the two leaves the
// comment hanging over a stranger and the declaration without its words; it
// has happened five times in one epic, by inserting "before the function"
// rather than "before its comment", and neither vet nor gofmt notices.
//
// The rule reads every Go file of the module, tests included: a comment whose
// first word is a name declared at the top level of its package must be the
// documentation of a declaration by that name.
func TestACommentStaysWithTheDeclarationItNames(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	byDir := map[string][]*ast.File{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		if generated(file) {
			return nil
		}
		byDir[filepath.Dir(path)] = append(byDir[filepath.Dir(path)], file)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	for _, files := range byDir {
		declared := map[string]bool{}
		for _, file := range files {
			for _, decl := range file.Decls {
				for _, name := range declNames(decl) {
					declared[name] = true
				}
			}
		}
		for _, file := range files {
			documents := map[*ast.CommentGroup][]string{}
			// A parenthesised block of constants or variables has no name of
			// its own, and its comment may open with the type they belong to.
			blocks := map[*ast.CommentGroup]bool{}
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Doc != nil {
						documents[d.Doc] = []string{d.Name.Name}
					}
				case *ast.GenDecl:
					if d.Doc != nil {
						documents[d.Doc] = declNames(d)
						blocks[d.Doc] = d.Lparen.IsValid()
					}
					for _, spec := range d.Specs {
						switch s := spec.(type) {
						case *ast.TypeSpec:
							if s.Doc != nil {
								documents[s.Doc] = []string{s.Name.Name}
							}
						case *ast.ValueSpec:
							if s.Doc != nil {
								documents[s.Doc] = identNames(s.Names)
							}
						}
					}
				}
			}
			for _, group := range file.Comments {
				word := firstWord(group)
				if word == "" || !declared[word] {
					continue
				}
				at := fset.Position(group.Pos())
				if names, ok := documents[group]; ok {
					if !contains(names, word) && !namedAfter(names, word) && !blocks[group] {
						t.Errorf("%s: the comment opens with %s but stands above %s",
							at, word, strings.Join(names, ", "))
					}
					continue
				}
				if insideADeclaration(file, group) {
					continue
				}
				t.Errorf("%s: the comment opens with %s and documents nothing - something was inserted between it and %s",
					at, word, word)
			}
		}
	}
}

// generated: a file a tool writes, and a tool's comments are its own business.
func generated(file *ast.File) bool {
	for _, group := range file.Comments {
		if group.Pos() > file.Package {
			return false
		}
		if strings.Contains(group.Text(), "Code generated") {
			return true
		}
	}
	return false
}

func declNames(decl ast.Decl) []string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		return []string{d.Name.Name}
	case *ast.GenDecl:
		var names []string
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				names = append(names, s.Name.Name)
			case *ast.ValueSpec:
				names = append(names, identNames(s.Names)...)
			}
		}
		return names
	}
	return nil
}

func identNames(idents []*ast.Ident) []string {
	var names []string
	for _, ident := range idents {
		names = append(names, ident.Name)
	}
	return names
}

var leadingIdentifier = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)(?:[\s.,:;()\[\]'"-]|$)`)

// firstWord is the identifier a comment opens with, if it opens with one.
// Directives (go:, nolint, nosemmgrep) are not comments about anything.
func firstWord(group *ast.CommentGroup) string {
	line := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(group.List[0].Text, "//"), "/*"))
	if strings.HasPrefix(group.List[0].Text, "//go:") || strings.HasPrefix(line, "nolint") ||
		strings.HasPrefix(line, "nosemgrep") || strings.HasPrefix(line, "+build") {
		return ""
	}
	m := leadingIdentifier.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	return m[1]
}

// insideADeclaration: a comment within a function body or a type's fields is
// not a top-level comment and says nothing about the rule.
func insideADeclaration(file *ast.File, group *ast.CommentGroup) bool {
	for _, decl := range file.Decls {
		if group.Pos() > decl.Pos() && group.End() < decl.End() {
			return true
		}
	}
	return false
}

// namedAfter: a test, benchmark or example is named after what it exercises,
// and its comment may open with that name.
func namedAfter(names []string, word string) bool {
	for _, n := range names {
		for _, prefix := range []string{"Test", "Benchmark", "Example"} {
			if strings.HasPrefix(n, prefix) && strings.Contains(strings.ToLower(n), strings.ToLower(word)) {
				return true
			}
		}
	}
	return false
}

func contains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}
