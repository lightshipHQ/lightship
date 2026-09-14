package traces_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The ClickHouse driver may be imported by exactly one package. A second query path is how a
// policy filter gets forgotten, so this is enforced by the build rather than by review.
//
// Test files are exempt: the property test drives ClickHouse directly on purpose, to compare the
// generated SQL against CEL's own evaluation.
func TestOnlyTheChokepointTalksToClickHouse(t *testing.T) {
	const driver = "github.com/ClickHouse/clickhouse-go"
	const allowed = "internal/traces"

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var offenders []string

	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "migrations" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if strings.HasPrefix(filepath.ToSlash(rel), allowed+"/") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		for _, imp := range f.Imports {
			if strings.Contains(imp.Path.Value, driver) {
				offenders = append(offenders, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Errorf("the ClickHouse driver may only be imported by %s, but it is imported by: %v\n"+
			"every query must go through the chokepoint so the policy filter cannot be omitted",
			allowed, offenders)
	}
}
