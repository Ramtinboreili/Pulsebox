// Package topology defines the compact graph model that the collector produces
// and the API/dashboard consume. It is deliberately free of Kubernetes types so
// the JSON contract stays small and stable.
package topology

import "time"

// Kinds used for graph nodes. These double as the JSON "kind" discriminator.
const (
	KindCluster     = "Cluster"
	KindNamespace   = "Namespace"
	KindNode        = "Node"
	KindDeployment  = "Deployment"
	KindStatefulSet = "StatefulSet"
	KindDaemonSet   = "DaemonSet"
	KindPod         = "Pod"
	KindPVC         = "PersistentVolumeClaim"
	KindPV          = "PersistentVolume"
	KindService     = "Service"
)

// Edge relationship kinds.
const (
	EdgeContains = "contains" // namespace -> workload, workload -> pod, cluster -> namespace/node
	EdgeRunsOn   = "runs-on"  // pod -> node
	EdgeMounts   = "mounts"   // pod -> pvc
	EdgeBacks    = "backs"    // pvc -> pv
	EdgeRoutes   = "routes"   // service -> pod
)

// Node is one vertex of the topology graph. Meta carries small, render-relevant
// extras (restart counts, GPU flag, images, etc.) as strings so the payload
// stays compact — full detail is fetched lazily via /api/resource.
type Node struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	Namespace string            `json:"namespace,omitempty"`
	Health    string            `json:"health"`
	Score     float64           `json:"score"`
	Reasons   []string          `json:"reasons,omitempty"`
	Meta      map[string]string `json:"meta,omitempty"`
}

// Edge is a directed relationship between two nodes.
type Edge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	Kind   string `json:"kind"`
}

// Graph is a full snapshot of cluster topology.
type Graph struct {
	Nodes     []Node    `json:"nodes"`
	Edges     []Edge    `json:"edges"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Message types pushed over the WebSocket stream.
const (
	MsgSnapshot = "snapshot" // full state, sent once on connect
	MsgDiff     = "diff"     // incremental change
)

// Diff is an incremental (or full, when Type==snapshot) change set. Removed*
// carry only IDs.
type Diff struct {
	Type        string    `json:"type"`
	UpsertNodes []Node    `json:"upsertNodes,omitempty"`
	UpsertEdges []Edge    `json:"upsertEdges,omitempty"`
	RemoveNodes []string  `json:"removeNodes,omitempty"`
	RemoveEdges []string  `json:"removeEdges,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
}

// Empty reports whether a diff carries no changes.
func (d Diff) Empty() bool {
	return len(d.UpsertNodes) == 0 && len(d.UpsertEdges) == 0 &&
		len(d.RemoveNodes) == 0 && len(d.RemoveEdges) == 0
}

// index builds lookup maps for a graph.
func (g Graph) index() (map[string]Node, map[string]Edge) {
	nodes := make(map[string]Node, len(g.Nodes))
	for _, n := range g.Nodes {
		nodes[n.ID] = n
	}
	edges := make(map[string]Edge, len(g.Edges))
	for _, e := range g.Edges {
		edges[e.ID] = e
	}
	return nodes, edges
}

// DiffAgainst computes the change set that turns prev into g.
func (g Graph) DiffAgainst(prev Graph) Diff {
	d := Diff{Type: MsgDiff, Timestamp: g.UpdatedAt}
	oldNodes, oldEdges := prev.index()
	newNodes, newEdges := g.index()

	for id, n := range newNodes {
		if o, ok := oldNodes[id]; !ok || !nodeEqual(o, n) {
			d.UpsertNodes = append(d.UpsertNodes, n)
		}
	}
	for id := range oldNodes {
		if _, ok := newNodes[id]; !ok {
			d.RemoveNodes = append(d.RemoveNodes, id)
		}
	}
	for id, e := range newEdges {
		if o, ok := oldEdges[id]; !ok || o != e {
			d.UpsertEdges = append(d.UpsertEdges, e)
		}
	}
	for id := range oldEdges {
		if _, ok := newEdges[id]; !ok {
			d.RemoveEdges = append(d.RemoveEdges, id)
		}
	}
	return d
}

// AsSnapshot renders the whole graph as a snapshot message.
func (g Graph) AsSnapshot() Diff {
	return Diff{
		Type:        MsgSnapshot,
		UpsertNodes: g.Nodes,
		UpsertEdges: g.Edges,
		Timestamp:   g.UpdatedAt,
	}
}

func nodeEqual(a, b Node) bool {
	if a.ID != b.ID || a.Kind != b.Kind || a.Name != b.Name || a.Namespace != b.Namespace ||
		a.Health != b.Health || a.Score != b.Score {
		return false
	}
	if len(a.Reasons) != len(b.Reasons) || len(a.Meta) != len(b.Meta) {
		return false
	}
	for i := range a.Reasons {
		if a.Reasons[i] != b.Reasons[i] {
			return false
		}
	}
	for k, v := range a.Meta {
		if b.Meta[k] != v {
			return false
		}
	}
	return true
}
