package runtime

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// decodeConfig turns the plugin's YAML block into a normalized config and the
// warnings raised over it: each dotted key that overrides a different nested
// value, then Normalize's. Keys
// the block omits keep their defaults, because yaml.v3 leaves absent fields
// untouched, and Normalize then clamps whatever the block did set. Duration
// fields accept Go duration strings such as "2m" and "15m".
//
// A top-level key holding a dot, such as affinity.ttl, sets the nested key it
// names, and wins over the same key written nested: the Management Center
// saves each field it renders as a flat key under its dotted name.
//
// An unparsable block yields the defaults with Enabled false, so a typo in the
// config file leaves the plugin loaded but inert rather than routing on a
// half-read configuration. A dotted key whose path runs through a value that
// is not a mapping, such as pace.shape beside pace: 3, is unparsable.
func decodeConfig(configYAML []byte) (model.Config, []string, error) {
	cfg := model.Defaults()
	warnings, err := decodeYAML(configYAML, &cfg)
	if err != nil {
		inert := model.Defaults()
		inert.Enabled = false
		return inert, nil, fmt.Errorf("parse plugin config: %w", err)
	}
	return cfg, append(warnings, cfg.Normalize()...), nil
}

// decodeYAML decodes a config block into cfg with its dotted keys expanded,
// and returns expandDottedKeys' warnings. An empty block, or one holding only
// comments, leaves cfg untouched.
func decodeYAML(configYAML []byte, cfg *model.Config) ([]string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(configYAML, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	var warnings []string
	if root := doc.Content[0]; root.Kind == yaml.MappingNode {
		var err error
		if warnings, err = expandDottedKeys(root); err != nil {
			return nil, err
		}
	}
	return warnings, doc.Decode(cfg)
}

// expandDottedKeys moves every key of the mapping that holds a dot into the
// nested mapping its dotted path names, creating the mappings on the way. The
// dotted keys apply after every other key, so each overrides its nested twin,
// and among dotted keys naming one path the last wins. A null on the path
// reads as an empty mapping.
//
// It returns a warning for each dotted key that overrides a nested value
// written differently, because an edit to that nested value has no effect.
func expandDottedKeys(root *yaml.Node) ([]string, error) {
	var dotted []*yaml.Node
	kept := make([]*yaml.Node, 0, len(root.Content))
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if key.Kind == yaml.ScalarNode && strings.Contains(key.Value, ".") {
			dotted = append(dotted, key, value)
			continue
		}
		kept = append(kept, key, value)
	}
	root.Content = kept

	var warnings []string
	fromDotted := make(map[*yaml.Node]bool, len(dotted)/2)
	for i := 0; i < len(dotted); i += 2 {
		name, value := dotted[i].Value, dotted[i+1]
		path := strings.Split(name, ".")
		m := root
		for _, segment := range path[:len(path)-1] {
			at := valueIndex(m, segment)
			switch {
			case at < 0:
				m.Content = append(m.Content, stringNode(segment), mappingNode())
				at = len(m.Content) - 1
			case m.Content[at].Kind == yaml.ScalarNode && m.Content[at].ShortTag() == "!!null":
				m.Content[at] = mappingNode()
			}
			if m.Content[at].Kind != yaml.MappingNode {
				return nil, fmt.Errorf("config key %s: %s is not a mapping", name, segment)
			}
			m = m.Content[at]
		}
		last := path[len(path)-1]
		if at := valueIndex(m, last); at >= 0 {
			if !fromDotted[m.Content[at]] && !sameNode(m.Content[at], value) {
				warnings = append(warnings, fmt.Sprintf("%s overrides %s; remove one", name, strings.Join(path, ": ")))
			}
			m.Content[at] = value
		} else {
			m.Content = append(m.Content, stringNode(last), value)
		}
		fromDotted[value] = true
	}
	return warnings, nil
}

// sameNode reports whether two YAML nodes are written alike: the same kind,
// scalar text and children, whatever their styles and positions.
func sameNode(a, b *yaml.Node) bool {
	if a.Kind != b.Kind || a.Value != b.Value || len(a.Content) != len(b.Content) {
		return false
	}
	for i := range a.Content {
		if !sameNode(a.Content[i], b.Content[i]) {
			return false
		}
	}
	return true
}

// valueIndex is the index in a mapping's Content of the value stored under a
// scalar key, or -1 when the key is absent.
func valueIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if k := m.Content[i]; k.Kind == yaml.ScalarNode && k.Value == key {
			return i + 1
		}
	}
	return -1
}

func stringNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func mappingNode() *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
}
