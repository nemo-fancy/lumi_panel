package subplane

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestBuildSkeletonFiltersAndCounts(t *testing.T) {
	candidates := []Node{
		{ID: 1, Name: "HK-01", Protocol: ProtoVLESS},
		{ID: 2, Name: "JP-01", Protocol: ProtoHysteria2},
		{ID: 3, Name: "SG-01", Protocol: ProtoTrojan},
		{ID: 4, Name: "HK-02", Protocol: ProtoLumi},
	}

	ir := BuildSkeleton(Selector{Profile: ProfileClashPremium, GroupIDs: []int64{1}}, candidates)

	if len(ir.Nodes) != 1 || ir.Nodes[0].ID != 3 {
		t.Fatalf("Clash Premium kept %+v, want only the Trojan node", ir.Nodes)
	}
	if ir.DroppedCount != 3 {
		t.Errorf("DroppedCount = %d, want 3", ir.DroppedCount)
	}

	ir = BuildSkeleton(Selector{Profile: ProfileMihomo, GroupIDs: []int64{1}}, candidates)
	if len(ir.Nodes) != 3 || ir.DroppedCount != 1 {
		t.Errorf("mihomo kept %d and dropped %d, want 3 and 1", len(ir.Nodes), ir.DroppedCount)
	}
}

func TestBuildSkeletonLeavesUserinfoEmpty(t *testing.T) {
	// Userinfo is per-user and the skeleton is shared. Filling it here would
	// mean one user's remaining traffic could be served to another from cache.
	ir := BuildSkeleton(Selector{Profile: ProfileMihomo}, []Node{{ID: 1, Protocol: ProtoTrojan}})
	if ir.Userinfo != (Userinfo{}) {
		t.Errorf("the shared skeleton carries per-user data: %+v", ir.Userinfo)
	}
}

// TestSelectorHasNoUserIdentity enforces §6.6 structurally.
//
// The rule is that node visibility is decided entirely by permission group,
// with no user-level exception. A user identifier reachable from the render
// entry point is all it takes for somebody to add "just this one user" later,
// at which point every group-keyed cache is either useless or serving user A
// the nodes from user B's group -- a privilege escalation wearing a
// performance bug's clothes.
//
// Asserting it against the source keeps the constraint true even for a field
// added in good faith years from now.
func TestSelectorHasNoUserIdentity(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "ir.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var checked bool
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Selector" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		checked = true

		for _, field := range st.Fields.List {
			for _, name := range field.Names {
				lower := strings.ToLower(name.Name)
				if strings.Contains(lower, "user") || lower == "uid" || lower == "email" {
					t.Errorf("Selector.%s identifies a user; node visibility is group-only (§6.6)", name.Name)
				}
			}
		}
		return false
	})

	if !checked {
		t.Fatal("Selector was not found in ir.go; this guard is no longer checking anything")
	}
}
