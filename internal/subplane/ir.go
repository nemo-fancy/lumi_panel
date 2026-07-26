package subplane

import "time"

// Visibility restricts which clients may be offered a node.
type Visibility string

const (
	// VisibleAll is offered to every client whose dialect can parse it.
	VisibleAll Visibility = "all"
	// VisibleLumiOnly is offered only to the native client.
	//
	// A machine carrying the in-house protocol also exposes a standard node,
	// so third-party clients still have a way in (§16.5). Without this check a
	// lumi_only node that happens to speak VLESS would pass the capability
	// filter and be rendered to everyone.
	VisibleLumiOnly Visibility = "lumi_only"
)

// Node is one renderable proxy entry in the intermediate representation.
type Node struct {
	ID       int64
	Name     string
	Protocol Protocol
	Host     string
	Port     int
	// GroupIDs are the permission groups this node belongs to. A node in no
	// group is visible to nobody.
	GroupIDs  []int64
	VisibleTo Visibility
	IsVisible bool
	// RateBP is the billing multiplier in basis points, surfaced so renderers
	// can annotate the display name (e.g. "HK-01 x1.5").
	RateBP   int16
	Tags     []string
	Settings map[string]any
}

// IR is the format-independent shape of a subscription response.
//
// Every renderer consumes this and nothing else. Without the layer, each new
// client format re-implements filtering, ordering, renaming and multiplier
// annotation -- which is precisely why subscription code in the V2Board
// lineage is the part nobody wants to touch (§6.2).
type IR struct {
	Nodes []Node
	// DroppedCount is how many nodes the user is entitled to but the client
	// cannot parse. It is rendered as an unconnectable informational entry
	// rather than omitted, so the user learns why their list is shorter
	// (§6.7).
	//
	// Nodes excluded by permission group are not counted: the user is not
	// entitled to them, so telling them the nodes exist would be a disclosure,
	// not an explanation.
	DroppedCount int
	// Userinfo populates the Subscription-Userinfo header. It is injected at
	// the last step because it is per-user, while everything above it is
	// per-permission-group and shared.
	Userinfo Userinfo
}

// Userinfo is the per-user traffic summary clients display.
type Userinfo struct {
	// Upload is always reported as zero. Clients in this ecosystem add upload
	// and download together for display, so filling both double-counts. The
	// convention is fixed and deviating from it looks like a billing bug to
	// the user (§6.8).
	Upload   int64
	Download int64
	Total    int64
	// Expire is the latest expiry across all active subscriptions. Taking the
	// earliest would make a client announce an expired subscription while a
	// data pack is still live.
	Expire time.Time
}

// Selector is everything the render pipeline is allowed to know about who is
// asking.
//
// There is no user ID in this struct, and that omission is the design (§6.6).
// Node visibility is decided entirely by permission group, with no user-level
// exception. Granting one user access to one node means creating a group
// containing that node; hiding a node means removing it from every group. The
// moment a user-level filter exists, any group-keyed render cache is either
// useless or serving user A the nodes from user B's group -- a privilege
// escalation wearing a caching bug's clothes.
//
// Keeping the user ID out of the signature makes that class of mistake fail to
// compile.
type Selector struct {
	Client Client
	// GroupIDs are the permission groups the requesting user belongs to. An
	// empty set selects no nodes.
	GroupIDs []int64
	// LumiOnly admits nodes marked lumi_only, which the native client alone
	// can use.
	LumiOnly bool
}

// BuildSkeleton assembles the shared, cacheable part of the IR: the node list
// after group, visibility and capability filtering.
//
// The result is identical for every user with the same selector, which is what
// makes it cacheable. Userinfo is deliberately left zeroed; the caller injects
// it per user afterwards.
//
// Filtering happens here rather than in the caller's query on purpose. An
// earlier version took the selector and applied only the capability filter,
// which meant a caller passing an unfiltered candidate list would hand every
// user every node -- and the type would have looked like it was preventing
// exactly that.
func BuildSkeleton(sel Selector, candidates []Node) IR {
	groups := make(map[int64]bool, len(sel.GroupIDs))
	for _, id := range sel.GroupIDs {
		groups[id] = true
	}

	ir := IR{Nodes: make([]Node, 0, len(candidates))}
	for _, n := range candidates {
		if !n.IsVisible {
			continue
		}
		if n.VisibleTo == VisibleLumiOnly && !sel.LumiOnly {
			continue
		}
		if !inAnyGroup(n.GroupIDs, groups) {
			continue
		}
		// Only nodes the user is entitled to reach this point, so the drop
		// count means what §6.7 says it means.
		if !Supports(sel.Client, n.Protocol) {
			ir.DroppedCount++
			continue
		}
		ir.Nodes = append(ir.Nodes, n)
	}
	return ir
}

func inAnyGroup(nodeGroups []int64, userGroups map[int64]bool) bool {
	for _, id := range nodeGroups {
		if userGroups[id] {
			return true
		}
	}
	return false
}
