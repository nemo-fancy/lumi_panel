package secret_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// secretPkg is the import path this guard tracks calls into.
const secretPkg = "github.com/nemo-fancy/lumi_panel/internal/platform/secret"

// outOfBandSenders are the packages permitted to unwrap a credential, because
// sending it out of band is their entire job.
//
// An allowlist rather than a denylist. The previous shape of this guard named
// the two packages that write responses, which left every package that
// *builds* a response payload -- service, store, the rest of platform --
// free to unwrap a credential and hand the string onward. Since §3.1 has
// payloads originating in service, that was the majority of the risk surface.
var outOfBandSenders = map[string]bool{
	"internal/platform/mail":   true,
	"internal/platform/notify": true,
	"internal/platform/secret": true,
	"internal/platform/config": true,
}

// TestDiscloseIsConfinedToOutOfBandSenders is the primary defence for the
// out-of-band credential rule (§12.1). The type system is the backstop; this
// is the check that fails the build.
//
// It parses rather than greps. The grep it replaces split each line on "//" to
// skip comments, which also truncated any line containing a URL -- so
//
//	resp.Link = "https://" + host + "/verify?t=" + secret.Disclose(tok)
//
// was invisible to it. That line is a magic login link in a response body,
// which is to say it is CVE-2026-39912 verbatim: the one thing the guard
// exists to catch was the one thing it could not see.
func TestDiscloseIsConfinedToOutOfBandSenders(t *testing.T) {
	root := filepath.Join("..", "..", "..")

	var scanned, offending int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "web", "node_modules", "bin", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if outOfBandSenders[filepath.ToSlash(filepath.Dir(rel))] {
			return nil
		}

		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}

		alias, imported := importAlias(f, secretPkg)
		if !imported {
			return nil
		}
		scanned++

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Disclose" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != alias {
				return true
			}
			offending++
			t.Errorf("%s:%d unwraps a credential outside an out-of-band sender",
				rel, fset.Position(call.Pos()).Line)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	t.Logf("scanned %d file(s) importing the secret package; %d violation(s)", scanned, offending)
}

// TestGuardActuallyInspectsSomething keeps the guard from passing green
// because it examined nothing.
//
// Its predecessor walked two directories, one of which did not exist on a
// fresh clone and the other of which never imported the package it was
// guarding, and reported success. There was no signal distinguishing "clean"
// from "not scanned", and none would have appeared for the entire period in
// which the API layer was being written.
func TestGuardActuallyInspectsSomething(t *testing.T) {
	root := filepath.Join("..", "..", "..")

	var goFiles int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return fs.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") {
			goFiles++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if goFiles < 5 {
		t.Fatalf("the guard walked %d Go files; it is not looking at the module", goFiles)
	}
}

// importAlias resolves the local name a file uses for an import path.
func importAlias(f *ast.File, path string) (string, bool) {
	for _, imp := range f.Imports {
		unquoted, err := strconv.Unquote(imp.Path.Value)
		if err != nil || unquoted != path {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name, true
		}
		return path[strings.LastIndex(path, "/")+1:], true
	}
	return "", false
}
