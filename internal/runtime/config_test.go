package runtime

import (
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// The Management Center saves an edited field as a flat top-level key under
// its dotted name, so every dotted key it renders has to reach the nested
// field it names.
func TestDottedKeysSetTheirNestedField(t *testing.T) {
	cfg, err := decodeConfig([]byte("enabled: true\n" +
		"affinity.ttl: 2h\n" +
		"pace.landing-target: 1.0\n" +
		"quota.poll-interval: 5m\n" +
		"web.enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Affinity.TTL != 2*time.Hour {
		t.Errorf("affinity.ttl = %v, want 2h", cfg.Affinity.TTL)
	}
	if cfg.Pace.LandingTarget != 1.0 {
		t.Errorf("pace.landing-target = %v, want 1.0", cfg.Pace.LandingTarget)
	}
	if cfg.Quota.PollInterval != 5*time.Minute {
		t.Errorf("quota.poll-interval = %v, want 5m", cfg.Quota.PollInterval)
	}
	if cfg.Web.Enabled {
		t.Error("web.enabled: false left the status app on")
	}
	if !cfg.Enabled {
		t.Error("a key without a dot stopped applying")
	}
}

func TestDottedAndNestedKeysMix(t *testing.T) {
	cfg, err := decodeConfig([]byte("quota:\n  max-staleness: 20m\n" +
		"quota.poll-interval: 5m\n" +
		"pace:\n  shape: sigmoid\n" +
		"affinity.ttl: 2h\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Quota.MaxStaleness != 20*time.Minute || cfg.Quota.PollInterval != 5*time.Minute {
		t.Errorf("quota = %v %v, want the nested 20m and the dotted 5m", cfg.Quota.MaxStaleness, cfg.Quota.PollInterval)
	}
	if cfg.Pace.Shape != model.ShapeSigmoid || cfg.Affinity.TTL != 2*time.Hour {
		t.Errorf("shape %q ttl %v, want sigmoid and 2h", cfg.Pace.Shape, cfg.Affinity.TTL)
	}
}

// The dotted key is the one the Management Center writes, so it is the edit
// the operator made last, wherever it sits in the block.
func TestADottedKeyOverridesItsNestedTwin(t *testing.T) {
	for name, block := range map[string]string{
		"dotted after nested":  "pace:\n  shape: power\npace.shape: sigmoid\n",
		"dotted before nested": "pace.shape: sigmoid\npace:\n  shape: power\n",
		"under a null parent":  "pace:\npace.shape: sigmoid\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := decodeConfig([]byte(block))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Pace.Shape != model.ShapeSigmoid {
				t.Errorf("shape = %q, want the dotted sigmoid", cfg.Pace.Shape)
			}
		})
	}
}

func TestADottedKeyThroughAScalarIsInvalid(t *testing.T) {
	cfg, err := decodeConfig([]byte("enabled: true\npace: 3\npace.shape: sigmoid\n"))
	if err == nil {
		t.Fatal("a dotted key under a scalar parsed")
	}
	if cfg.Enabled {
		t.Error("an invalid block left the plugin enabled")
	}
}

func TestAnEmptyBlockDecodesToTheDefaults(t *testing.T) {
	want := model.Defaults()
	want.Normalize()
	for _, block := range []string{"", "\n", "# nothing set\n", "null\n"} {
		cfg, err := decodeConfig([]byte(block))
		if err != nil {
			t.Errorf("%q: %v", block, err)
			continue
		}
		if cfg.Pace != want.Pace || cfg.Quota != want.Quota || cfg.Affinity != want.Affinity || cfg.Web != want.Web {
			t.Errorf("%q decoded to %+v, want the defaults", block, cfg)
		}
	}
}
