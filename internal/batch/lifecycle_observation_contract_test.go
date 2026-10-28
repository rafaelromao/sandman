package batch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

func TestLifecycleObservation_NoLegacyDecisionAliases(t *testing.T) {
	legacyNames := map[string]bool{
		"handleLifecycleDecision":               true,
		"handleLifecycleDecisionForAttempt":     true,
		"observeLifecycle":                      true,
		"observeLifecycleTupleForTest":          true,
		"continuityDecision":                    true,
		"lifecycleDecisionForTest":              true,
		"lifecycleDecisionWithHostPathsForTest": true,
		"lifecycleDecisionAtHeadForTest":        true,
		"mergedLifecycleDecisionForTest":        true,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	observerMethods := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if legacyNames[function.Name.Name] {
				t.Errorf("legacy lifecycle decision alias %q remains in %s", function.Name.Name, name)
			}
			if function.Name.Name == "Observe" && function.Recv != nil {
				if receiver, ok := function.Recv.List[0].Type.(*ast.StarExpr); ok {
					if identifier, ok := receiver.X.(*ast.Ident); ok && identifier.Name == "lifecycleObserver" {
						observerMethods++
					}
				}
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			declaration, ok := node.(*ast.TypeSpec)
			if !ok || declaration.Name.Name != "lifecycleObservation" {
				return true
			}
			interfaceType, ok := declaration.Type.(*ast.InterfaceType)
			if !ok || len(interfaceType.Methods.List) != 1 || len(interfaceType.Methods.List[0].Names) != 1 || interfaceType.Methods.List[0].Names[0].Name != "Observe" {
				t.Errorf("lifecycleObservation must expose only the typed Observe operation")
			}
			return true
		})
	}
	if observerMethods != 1 {
		t.Errorf("lifecycleObserver Observe implementations = %d, want exactly one", observerMethods)
	}
}
