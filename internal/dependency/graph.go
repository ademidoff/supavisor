package dependency

import (
	"fmt"
	"slices"
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

// GetDependents returns all processes that depend on the given process
func (g *Graph) GetDependents(name string) []string {
	node, exists := g.nodes[name]
	if !exists {
		return []string{}
	}
	return node.Dependents
}

// GetDependencies returns all processes that the given process depends on
func (g *Graph) GetDependencies(name string) []string {
	node, exists := g.nodes[name]
	if !exists {
		return []string{}
	}
	return node.Dependencies
}

// HasNode checks if a node exists in the graph
func (g *Graph) HasNode(name string) bool {
	_, exists := g.nodes[name]
	return exists
}

// AllNodes returns all node names in the graph
func (g *Graph) AllNodes() []string {
	nodes := make([]string, 0, len(g.nodes))
	for name := range g.nodes {
		nodes = append(nodes, name)
	}
	return nodes
}
