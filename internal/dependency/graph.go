package dependency

import (
	"fmt"
	"slices"
	"sort"
)

// Graph represents a directed graph of process dependencies
type Graph struct {
	nodes map[string]*Node
}

// Node represents a node in the dependency graph
type Node struct {
	Name         string
	Dependencies []string
	Dependents   []string
}

// NewGraph creates a new dependency graph
func NewGraph() *Graph {
	return &Graph{
		nodes: make(map[string]*Node),
	}
}

// AddNode adds a node to the graph
func (g *Graph) AddNode(name string, dependencies []string) {
	// If node already exists, update its dependencies
	if existingNode, exists := g.nodes[name]; exists {
		// Remove old dependent relationships
		for _, oldDep := range existingNode.Dependencies {
			if depNode, exists := g.nodes[oldDep]; exists {
				// Remove name from oldDep's dependents
				newDependents := make([]string, 0, len(depNode.Dependents))
				for _, d := range depNode.Dependents {
					if d != name {
						newDependents = append(newDependents, d)
					}
				}
				depNode.Dependents = newDependents
			}
		}
		existingNode.Dependencies = dependencies
	} else {
		node := &Node{
			Name:         name,
			Dependencies: dependencies,
			Dependents:   make([]string, 0),
		}
		g.nodes[name] = node
	}

	// Update dependents for each dependency
	node := g.nodes[name]
	for _, dep := range dependencies {
		if depNode, exists := g.nodes[dep]; exists {
			// Check if name is already in dependents
			found := slices.Contains(depNode.Dependents, name)
			if !found {
				depNode.Dependents = append(depNode.Dependents, name)
			}
		}
	}

	// Also check if any existing nodes depend on this newly added node
	for otherName, otherNode := range g.nodes {
		if otherName == name {
			continue
		}
		for _, dep := range otherNode.Dependencies {
			if dep == name {
				// otherNode depends on name, so name should have otherName as dependent
				found := slices.Contains(node.Dependents, otherName)
				if !found {
					node.Dependents = append(node.Dependents, otherName)
				}
			}
		}
	}
}

// TopologicalSort returns a topological ordering of nodes
// Returns an error if a circular dependency is detected
func (g *Graph) TopologicalSort() ([]string, error) {
	// Calculate in-degrees
	inDegree := make(map[string]int)

	for name, node := range g.nodes {
		inDegree[name] = 0
		for _, dep := range node.Dependencies {
			if _, exists := g.nodes[dep]; exists {
				inDegree[name]++
			}
		}
	}

	// Find all nodes with in-degree 0
	queue := make([]string, 0, len(g.nodes))
	for name, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, name)
		}
	}

	result := []string{}
	processed := 0

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		result = append(result, current)
		processed++

		// Reduce in-degree for all dependents
		node := g.nodes[current]
		for _, dependent := range node.Dependents {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}

	// If we didn't process all nodes, there's a cycle
	if processed != len(g.nodes) {
		return nil, fmt.Errorf("circular dependency detected")
	}

	return result, nil
}

// Tiers groups nodes so that everything in a tier depends only on nodes in
// earlier tiers. Startup works forwards through the tiers, shutdown backwards,
// and everything within one tier is independent of its peers.
// Returns an error if a circular dependency is detected.
func (g *Graph) Tiers() ([][]string, error) {
	inDegree := make(map[string]int, len(g.nodes))
	for name, node := range g.nodes {
		inDegree[name] = 0
		for _, dep := range node.Dependencies {
			if _, exists := g.nodes[dep]; exists {
				inDegree[name]++
			}
		}
	}

	current := make([]string, 0, len(g.nodes))
	for name, degree := range inDegree {
		if degree == 0 {
			current = append(current, name)
		}
	}

	tiers := [][]string{}
	placed := 0

	for len(current) > 0 {
		sort.Strings(current)
		tiers = append(tiers, current)
		placed += len(current)

		next := make([]string, 0, len(g.nodes))
		for _, name := range current {
			for _, dependent := range g.nodes[name].Dependents {
				inDegree[dependent]--
				if inDegree[dependent] == 0 {
					next = append(next, dependent)
				}
			}
		}
		current = next
	}

	if placed != len(g.nodes) {
		return nil, fmt.Errorf("circular dependency detected")
	}

	return tiers, nil
}

// GetDependencies returns all processes that the given process depends on
func (g *Graph) GetDependencies(name string) []string {
	node, exists := g.nodes[name]
	if !exists {
		return []string{}
	}
	return node.Dependencies
}
