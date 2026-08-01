package dependency

import (
	"testing"
)

func TestGraph_TopologicalSort(t *testing.T) {
	tests := []struct {
		nodes      map[string][]string
		checkOrder func([]string) bool
		name       string
		wantErr    bool
	}{
		{
			name: "simple linear dependencies",
			nodes: map[string][]string{
				"a": {},
				"b": {"a"},
				"c": {"b"},
			},
			wantErr: false,
			checkOrder: func(order []string) bool {
				// a should come before b, b before c
				return indexOf(order, "a") < indexOf(order, "b") &&
					indexOf(order, "b") < indexOf(order, "c")
			},
		},
		{
			name: "no dependencies",
			nodes: map[string][]string{
				"a": {},
				"b": {},
				"c": {},
			},
			wantErr: false,
			checkOrder: func(order []string) bool {
				return len(order) == 3
			},
		},
		{
			name: "circular dependency",
			nodes: map[string][]string{
				"a": {"b"},
				"b": {"a"},
			},
			wantErr: true,
		},
		{
			name: "complex dependencies",
			nodes: map[string][]string{
				"a": {},
				"b": {"a"},
				"c": {"a"},
				"d": {"b", "c"},
			},
			wantErr: false,
			checkOrder: func(order []string) bool {
				// a should come first, d should come last
				return indexOf(order, "a") < indexOf(order, "b") &&
					indexOf(order, "a") < indexOf(order, "c") &&
					indexOf(order, "b") < indexOf(order, "d") &&
					indexOf(order, "c") < indexOf(order, "d")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGraph()
			for name, deps := range tt.nodes {
				g.AddNode(name, deps)
			}

			order, err := g.TopologicalSort()
			if (err != nil) != tt.wantErr {
				t.Errorf("TopologicalSort() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if len(order) != len(tt.nodes) {
					t.Errorf("Expected %d nodes in order, got %d", len(tt.nodes), len(order))
				}
				if tt.checkOrder != nil && !tt.checkOrder(order) {
					t.Errorf("Order check failed for order: %v", order)
				}
			}
		})
	}
}

func TestGraph_GetDependencies(t *testing.T) {
	g := NewGraph()
	g.AddNode("a", []string{})
	g.AddNode("b", []string{"a"})
	g.AddNode("c", []string{"a", "b"})

	deps := g.GetDependencies("c")
	if len(deps) != 2 {
		t.Errorf("Expected 2 dependencies for 'c', got %d", len(deps))
	}

	if !contains(deps, "a") || !contains(deps, "b") {
		t.Errorf("Expected dependencies to contain 'a' and 'b', got %v", deps)
	}
}

func indexOf(slice []string, item string) int {
	for i, v := range slice {
		if v == item {
			return i
		}
	}
	return -1
}

func contains(slice []string, item string) bool {
	return indexOf(slice, item) != -1
}

func TestTiers(t *testing.T) {
	g := NewGraph()
	g.AddNode("db", nil)
	g.AddNode("cache", nil)
	g.AddNode("api", []string{"db", "cache"})
	g.AddNode("worker", []string{"api"})

	tiers, err := g.Tiers()
	if err != nil {
		t.Fatalf("Tiers failed: %v", err)
	}

	expected := [][]string{{"cache", "db"}, {"api"}, {"worker"}}
	if len(tiers) != len(expected) {
		t.Fatalf("Expected %d tiers, got %d: %v", len(expected), len(tiers), tiers)
	}
	for i := range expected {
		if len(tiers[i]) != len(expected[i]) {
			t.Fatalf("Tier %d = %v, expected %v", i, tiers[i], expected[i])
		}
		for j := range expected[i] {
			if tiers[i][j] != expected[i][j] {
				t.Errorf("Tier %d = %v, expected %v", i, tiers[i], expected[i])
			}
		}
	}
}

func TestTiers_DetectsCycles(t *testing.T) {
	g := NewGraph()
	g.AddNode("a", []string{"b"})
	g.AddNode("b", []string{"a"})

	if _, err := g.Tiers(); err == nil {
		t.Error("Expected a circular dependency error")
	}
}

func TestTiers_IndependentNodesShareATier(t *testing.T) {
	g := NewGraph()
	g.AddNode("a", nil)
	g.AddNode("b", nil)
	g.AddNode("c", nil)

	tiers, err := g.Tiers()
	if err != nil {
		t.Fatalf("Tiers failed: %v", err)
	}
	if len(tiers) != 1 || len(tiers[0]) != 3 {
		t.Errorf("Independent nodes should share one tier, got %v", tiers)
	}
}
