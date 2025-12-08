package dependency

import (
	"testing"
)

func TestGraph_TopologicalSort(t *testing.T) {
	tests := []struct {
		name       string
		nodes      map[string][]string
		wantErr    bool
		checkOrder func([]string) bool
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

func TestGraph_GetDependents(t *testing.T) {
	g := NewGraph()
	g.AddNode("a", []string{})
	g.AddNode("b", []string{"a"})
	g.AddNode("c", []string{"a"})

	dependents := g.GetDependents("a")
	if len(dependents) != 2 {
		t.Errorf("Expected 2 dependents for 'a', got %d", len(dependents))
	}

	if !contains(dependents, "b") || !contains(dependents, "c") {
		t.Errorf("Expected dependents to contain 'b' and 'c', got %v", dependents)
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
