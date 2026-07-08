package topology

import "testing"

func node(id, health string, score float64) Node {
	return Node{ID: id, Kind: KindPod, Name: id, Health: health, Score: score}
}

func TestDiffAgainst(t *testing.T) {
	prev := Graph{
		Nodes: []Node{node("a", "healthy", 1), node("b", "healthy", 1)},
		Edges: []Edge{{ID: "e1", Source: "a", Target: "b", Kind: EdgeContains}},
	}
	next := Graph{
		Nodes: []Node{node("a", "unhealthy", 0), node("c", "healthy", 1)}, // a changed, b removed, c added
		Edges: []Edge{{ID: "e2", Source: "a", Target: "c", Kind: EdgeContains}},
	}

	d := next.DiffAgainst(prev)

	if len(d.UpsertNodes) != 2 { // a (changed) + c (new)
		t.Fatalf("UpsertNodes = %d, want 2 (%v)", len(d.UpsertNodes), d.UpsertNodes)
	}
	if len(d.RemoveNodes) != 1 || d.RemoveNodes[0] != "b" {
		t.Fatalf("RemoveNodes = %v, want [b]", d.RemoveNodes)
	}
	if len(d.UpsertEdges) != 1 || d.UpsertEdges[0].ID != "e2" {
		t.Fatalf("UpsertEdges = %v, want [e2]", d.UpsertEdges)
	}
	if len(d.RemoveEdges) != 1 || d.RemoveEdges[0] != "e1" {
		t.Fatalf("RemoveEdges = %v, want [e1]", d.RemoveEdges)
	}
}

func TestDiffNoChange(t *testing.T) {
	g := Graph{Nodes: []Node{node("a", "healthy", 1)}}
	if d := g.DiffAgainst(g); !d.Empty() {
		t.Fatalf("identical graphs should diff empty, got %+v", d)
	}
}
