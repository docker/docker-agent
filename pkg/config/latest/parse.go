package latest

import "github.com/goccy/go-yaml"

func Register(parsers map[string]func([]byte) (any, error), _ *[]func(any, []byte) (any, error)) {
	parsers[Version] = func(d []byte) (any, error) { return parse(d) }
}

func parse(data []byte) (Config, error) {
	var cfg Config
	err := yaml.UnmarshalWithOptions(data, &cfg, yaml.Strict())
	return cfg, err
}

// ParseRaw decodes a latest-version YAML document without running Config.Validate
// so config.Load can merge global user definitions before cross-section validation.
func ParseRaw(data []byte) (Config, error) {
	type alias Config
	var raw alias
	err := yaml.UnmarshalWithOptions(data, &raw, yaml.Strict())
	return Config(raw), err
}
