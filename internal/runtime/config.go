package runtime

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// decodeConfig turns the plugin's YAML block into a normalized config. Keys
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
func decodeConfig(configYAML []byte) (model.Config, error) {
	cfg := model.Defaults()
	if err := decodeYAML(configYAML, &cfg); err != nil {
		inert := model.Defaults()
		inert.Enabled = false
		return inert, fmt.Errorf("parse plugin config: %w", err)
	}
	cfg.Normalize()
	return cfg, nil
}

// decodeYAML decodes a config block into cfg with its dotted keys expanded.
// An empty block, or one holding only comments, leaves cfg untouched.
func decodeYAML(configYAML []byte, cfg *model.Config) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(configYAML, &doc); err != nil {
		return err
	}
	if len(doc.Content) == 0 {
		return nil
	}
	if root := doc.Content[0]; root.Kind == yaml.MappingNode {
		if err := expandDottedKeys(root); err != nil {
			return err
		}
	}
	return doc.Decode(cfg)
}

// expandDottedKeys moves every key of the mapping that holds a dot into the
// nested mapping its dotted path names, creating the mappings on the way. The
// dotted keys apply after every other key, so each overrides its nested twin,
// and among dotted keys naming one path the last wins. A null on the path
// reads as an empty mapping.
func expandDottedKeys(root *yaml.Node) error {
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
				return fmt.Errorf("config key %s: %s is not a mapping", name, segment)
			}
			m = m.Content[at]
		}
		last := path[len(path)-1]
		if at := valueIndex(m, last); at >= 0 {
			m.Content[at] = value
		} else {
			m.Content = append(m.Content, stringNode(last), value)
		}
	}
	return nil
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
