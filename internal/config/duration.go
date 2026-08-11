package config

import (
	"fmt"
	"time"

	"go.yaml.in/yaml/v3"
)

// Duration is a YAML-friendly time.Duration.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a string")
	}

	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}

	*d = Duration(parsed)
	return nil
}

func (d Duration) Value() time.Duration {
	return time.Duration(d)
}

func (d Duration) String() string {
	return time.Duration(d).String()
}
