package runtime

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// decodeConfig turns the plugin's YAML block into a normalized config. Keys
// the block omits keep their defaults, because yaml.v3 leaves absent fields
// untouched, and Normalize then clamps whatever the block did set. Duration
// fields accept Go duration strings such as "2m" and "15m".
//
// An unparsable block yields the defaults with Enabled false, so a typo in the
// config file leaves the plugin loaded but inert rather than routing on a
// half-read configuration.
func decodeConfig(configYAML []byte) (model.Config, error) {
	cfg := model.Defaults()
	if len(configYAML) > 0 {
		if err := yaml.Unmarshal(configYAML, &cfg); err != nil {
			inert := model.Defaults()
			inert.Enabled = false
			return inert, fmt.Errorf("parse plugin config: %w", err)
		}
	}
	cfg.Normalize()
	return cfg, nil
}
