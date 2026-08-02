package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// DependencyCondition is what a dependency has to reach before a program that
// depends on it may start.
type DependencyCondition string

const (
	// ConditionStarted is satisfied once the dependency is RUNNING, which says
	// its process is alive but nothing about whether it can serve yet.
	ConditionStarted DependencyCondition = "started"

	// ConditionHealthy also requires the dependency's health check to pass,
	// which is what readiness means for a program that initializes after its
	// process is up.
	ConditionHealthy DependencyCondition = "healthy"
)

// Dependency is one entry of a program's depends_on
type Dependency struct {
	Name      string
	Condition DependencyCondition
}

// dependsOnFile accepts both depends_on forms: a list of program names, which
// waits for each of them to be RUNNING, and a mapping of program name to
// condition, which can additionally wait for a health check.
type dependsOnFile []Dependency

// UnmarshalYAML decodes either depends_on form
func (d *dependsOnFile) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.SequenceNode {
		var names []string
		if err := node.Decode(&names); err != nil {
			return fmt.Errorf("invalid depends_on: %w", err)
		}
		deps := make([]Dependency, 0, len(names))
		for _, name := range names {
			deps = append(deps, Dependency{Name: name, Condition: ConditionStarted})
		}
		*d = deps
		return nil
	}

	if node.Kind == yaml.MappingNode {
		deps, err := parseDependencyMapping(node)
		if err != nil {
			return err
		}
		*d = deps
		return nil
	}

	return fmt.Errorf("invalid depends_on: expected a list of program names, or a mapping of program name to condition")
}

// parseDependencyMapping reads the mapping form. The nodes are walked by hand
// rather than decoded into a struct so that an unknown key is still rejected:
// a yaml.Node decodes without the strict mode the config decoder was given.
func parseDependencyMapping(node *yaml.Node) ([]Dependency, error) {
	deps := make([]Dependency, 0, len(node.Content)/2)

	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		dep := Dependency{Name: key.Value, Condition: ConditionStarted}

		switch {
		// A bare name with nothing under it keeps the default condition
		case value.Tag == "!!null":
		case value.Kind == yaml.MappingNode:
			condition, err := parseDependencyCondition(key.Value, value)
			if err != nil {
				return nil, err
			}
			dep.Condition = condition
		default:
			return nil, fmt.Errorf("invalid depends_on entry %s: expected a mapping with a condition key", key.Value)
		}

		deps = append(deps, dep)
	}

	return deps, nil
}

// parseDependencyCondition reads the settings of one mapping-form entry
func parseDependencyCondition(name string, node *yaml.Node) (DependencyCondition, error) {
	condition := ConditionStarted

	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Value != "condition" {
			return "", fmt.Errorf("invalid depends_on entry %s: unknown key %s", name, key.Value)
		}

		switch DependencyCondition(value.Value) {
		case ConditionStarted, ConditionHealthy:
			condition = DependencyCondition(value.Value)
		default:
			return "", fmt.Errorf("invalid depends_on condition for %s: %s (must be started or healthy)", name, value.Value)
		}
	}

	return condition, nil
}
