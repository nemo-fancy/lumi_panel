package subplane

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func node(id int64, proto Protocol, groups ...int64) Node {
	return Node{ID: id, Protocol: proto, GroupIDs: groups, VisibleTo: VisibleAll, IsVisible: true}
}

func idsIn(ir IR) []int64 {
	out := make([]int64, len(ir.Nodes))
	for i, n := range ir.Nodes {
		out[i] = n.ID
	}
	return out
}

func TestBuildSkeletonFiltersByCapability(t *testing.T) {
	candidates := []Node{
		node(1, ProtoVLESS, 1),
		node(2, ProtoHysteria2, 1),
		node(3, ProtoTrojan, 1),
		node(4, ProtoLumi, 1),
	}

	ir := BuildSkeleton(Selector{Client: newest(ProfileClashPremium), GroupIDs: []int64{1}}, candidates)
	if got := idsIn(ir); len(got) != 1 || got[0] != 3 {
		t.Fatalf("Clash Premium kept %v, want only the Trojan node", got)
	}
	if ir.DroppedCount != 3 {
		t.Errorf("DroppedCount = %d, want 3", ir.DroppedCount)
	}

	ir = BuildSkeleton(Selector{Client: newest(ProfileMihomo), GroupIDs: []int64{1}}, candidates)
	if len(ir.Nodes) != 3 || ir.DroppedCount != 1 {
		t.Errorf("mihomo kept %d and dropped %d, want 3 and 1", len(ir.Nodes), ir.DroppedCount)
	}
}

// TestBuildSkeletonEnforcesPermissionGroups is the §6.6 guarantee, and it is a
// privilege boundary rather than a filtering convenience: a node the user's
// groups do not contain must not appear, whatever their client can parse.
//
// An earlier version of BuildSkeleton read only the profile and ignored
// GroupIDs entirely, so a caller passing an unfiltered candidate list handed
// every user every node -- while the Selector type looked like it was
// preventing exactly that.
func TestBuildSkeletonEnforcesPermissionGroups(t *testing.T) {
	candidates := []Node{
		node(1, ProtoTrojan, 1),
		node(2, ProtoTrojan, 2),
		node(3, ProtoTrojan, 1, 2),
		node(4, ProtoTrojan), // in no group at all
	}

	ir := BuildSkeleton(Selector{Client: newest(ProfileMihomo), GroupIDs: []int64{1}}, candidates)
	got := idsIn(ir)
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("kept %v, want [1 3]", got)
	}

	// A node belonging to no group reaches nobody.
	ir = BuildSkeleton(Selector{Client: newest(ProfileMihomo), GroupIDs: []int64{1, 2, 3}}, candidates)
	for _, n := range ir.Nodes {
		if n.ID == 4 {
			t.Error("a node in no permission group was rendered")
		}
	}

	// No groups, no nodes.
	if ir := BuildSkeleton(Selector{Client: newest(ProfileMihomo)}, candidates); len(ir.Nodes) != 0 {
		t.Errorf("a user in no group received %d nodes", len(ir.Nodes))
	}
}

// TestGroupExclusionsAreNotCounted keeps the drop count honest. It exists to
// explain a shorter list to the user; counting nodes they are not entitled to
// would disclose that other people's nodes exist.
func TestGroupExclusionsAreNotCounted(t *testing.T) {
	candidates := []Node{
		node(1, ProtoTrojan, 1),
		node(2, ProtoTrojan, 2),
		node(3, ProtoAnyTLS, 1),
	}

	ir := BuildSkeleton(Selector{Client: newest(ProfileClashPremium), GroupIDs: []int64{1}}, candidates)
	if ir.DroppedCount != 1 {
		t.Errorf("DroppedCount = %d, want 1 (the AnyTLS node the client cannot parse, not the other group's)", ir.DroppedCount)
	}
}

// TestLumiOnlyNodesAreNotRenderedToThirdParties covers §16.5. A machine
// carrying the in-house protocol dual-listens, so its lumi_only node may speak
// a standard protocol -- which means the capability filter alone would pass it
// straight through to every client.
func TestLumiOnlyNodesAreNotRenderedToThirdParties(t *testing.T) {
	lumiOnly := node(1, ProtoVLESS, 1)
	lumiOnly.VisibleTo = VisibleLumiOnly
	candidates := []Node{lumiOnly, node(2, ProtoVLESS, 1)}

	ir := BuildSkeleton(Selector{Client: newest(ProfileMihomo), GroupIDs: []int64{1}}, candidates)
	if got := idsIn(ir); len(got) != 1 || got[0] != 2 {
		t.Fatalf("kept %v, want only the publicly visible node", got)
	}

	ir = BuildSkeleton(Selector{Client: newest(ProfileMihomo), GroupIDs: []int64{1}, LumiOnly: true}, candidates)
	if len(ir.Nodes) != 2 {
		t.Errorf("the native client received %d nodes, want 2", len(ir.Nodes))
	}
}

func TestBuildSkeletonHonoursIsVisible(t *testing.T) {
	hidden := node(1, ProtoTrojan, 1)
	hidden.IsVisible = false

	ir := BuildSkeleton(Selector{Client: newest(ProfileMihomo), GroupIDs: []int64{1}}, []Node{hidden})
	if len(ir.Nodes) != 0 {
		t.Error("a node marked not visible was rendered")
	}
	if ir.DroppedCount != 0 {
		t.Error("an administratively hidden node was counted as a client capability drop")
	}
}

func TestBuildSkeletonLeavesUserinfoEmpty(t *testing.T) {
	// Userinfo is per-user and the skeleton is shared. Filling it here would
	// mean one user's remaining traffic could be served to another from cache.
	ir := BuildSkeleton(Selector{Client: newest(ProfileMihomo), GroupIDs: []int64{1}}, []Node{node(1, ProtoTrojan, 1)})
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
// added in good faith years from now. The name list is deliberately broad:
// the point is to make any per-subject identifier fail here and force the
// author to argue for it.
func TestSelectorHasNoUserIdentity(t *testing.T) {
	forbidden := []string{"user", "account", "subscriber", "owner", "customer", "member", "uid", "email", "token"}

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
				for _, bad := range forbidden {
					if strings.Contains(lower, bad) {
						t.Errorf("Selector.%s identifies a subject; node visibility is group-only (§6.6)", name.Name)
					}
				}
			}
		}
		return false
	})

	if !checked {
		t.Fatal("Selector was not found in ir.go; this guard is no longer checking anything")
	}
}
