package subplane

import "time"

// Node is one renderable proxy entry in the intermediate representation.
type Node struct {
	ID       int64
	Name     string
	Protocol Protocol
	Host     string
	Port     int
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
	// DroppedCount is how many nodes the client cannot parse. It is rendered
	// as an unconnectable informational entry rather than omitted, so the
	// user learns why their list is shorter (§6.7).
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
// Node visibility is decided entirely by permission group: granting one user
// access to one node means creating a group containing that node, and hiding a
// node means removing it from every group. The moment a user-level filter
// exists, any group-keyed render cache is either wrong or useless -- and
// "wrong" here means user A receiving user B's nodes, which is a privilege
// escalation rather than a caching bug.
//
// Keeping the user ID out of the signature makes that class of mistake fail to
// compile.
type Selector struct {
	Profile  Profile
	GroupIDs []int64
	// LumiOnly admits nodes marked visible_to = lumi_only, which the native
	// client alone can use.
	LumiOnly bool
}

// BuildSkeleton assembles the shared, cacheable part of the IR: the node list
// after group and capability filtering.
//
// The result is identical for every user with the same selector, which is what
// makes it cacheable. Userinfo is deliberately left zeroed; the caller injects
// it per user afterwards.
func BuildSkeleton(sel Selector, candidates []Node) IR {
	ir := IR{Nodes: make([]Node, 0, len(candidates))}
	for _, n := range candidates {
		if !Supports(sel.Profile, n.Protocol) {
			ir.DroppedCount++
			continue
		}
		ir.Nodes = append(ir.Nodes, n)
	}
	return ir
}
