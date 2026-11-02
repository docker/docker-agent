package main

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
	"gotest.tools/v3/golden"
)

var samplePkg = reflect.TypeFor[SampleEvent]().PkgPath()

// SampleContext is embedded by every sample event.
type SampleContext struct {
	AgentName string    `json:"agent_name,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// SampleEvent exercises the encoding/json rules the emitter mirrors.
type SampleEvent struct {
	SampleContext
	*sampleExtra

	Type string `json:"type"`
	// AgentName shadows the embedded one, so it is required.
	AgentName string         `json:"agent_name"`
	Items     []SampleItem   `json:"items"`
	Tags      []string       `json:"tags,omitempty"`
	Next      *SampleItem    `json:"next,omitempty"`
	Parent    *SampleItem    `json:"parent"`
	Meta      map[string]any `json:"meta"`
	Kind      SampleKind     `json:"kind"`
	Wait      time.Duration  `json:"wait,omitempty"`
	Raw       []byte         `json:"raw,omitempty"`
	Count     int            `json:"count,string"`
	Ratio     *float64       `json:"ratio,string"`
	Nested    struct {
		Depth int `json:"depth"`
	} `json:"nested"`
	Always  SampleItem   `json:"always,omitempty"` //nolint:modernize // encoding/json ignores omitempty on structs; the emitter must too
	Maybe   SampleItem   `json:"maybe,omitzero"`
	Custom  SampleCustom `json:"custom"`
	Untyped any          `json:"untyped"`
	NoTag   bool
	Hidden  string `json:"-"`
}

type sampleExtra struct {
	Extra string `json:"extra"`
}

// SampleKind is rendered as the union of its constants.
type SampleKind string

const (
	SampleKindA SampleKind = "a"
	SampleKindB SampleKind = "it's b"
	SampleKindC            = SampleKind("c")
	SampleKindD SampleKind = SampleKindA + "d"
)

// SampleOpen has no constants.
type SampleOpen string

// SampleItem is recursive.
type SampleItem struct {
	Children []SampleItem `json:"children,omitempty"`
}

// SampleCustom has its own encoding.
type SampleCustom struct{ value string }

func (c SampleCustom) MarshalJSON() ([]byte, error) { return json.Marshal(c.value) }

func sampleConfig() *config {
	return &config{
		events:  map[string]reflect.Type{"sample": reflect.TypeFor[SampleEvent]()},
		modules: map[string]string{samplePkg: "sample"},
		enums:   map[reflect.Type][]string{reflect.TypeFor[SampleKind](): {""}},
		overrides: map[reflect.Type]func(*builder) (*jsonschema.Schema, error){
			reflect.TypeFor[SampleCustom](): func(*builder) (*jsonschema.Schema, error) {
				return &jsonschema.Schema{Type: "string"}, nil
			},
		},
	}
}

// sampleSource loads this package's test variant, which declares the
// sample types.
var sampleSource = sync.OnceValues(func() (*goSource, error) {
	return loadPackages(&packages.Config{Mode: loadMode, Tests: true}, samplePkg)
})

func sampleBuild(t *testing.T, cfg *config) ([]*def, error) {
	t.Helper()
	src, err := sampleSource()
	require.NoError(t, err)
	return build(cfg, src)
}

func TestEmitTypeScriptGolden(t *testing.T) {
	t.Parallel()

	defs, err := sampleBuild(t, sampleConfig())
	require.NoError(t, err)
	files, err := emitTypeScript(defs)
	require.NoError(t, err)

	assert.Equal(t, []string{"sample.ts"}, sortedKeys(files))
	golden.Assert(t, files["sample.ts"], "sample.ts.golden")
}

func TestEmitIndex(t *testing.T) {
	t.Parallel()

	got := emitIndex(map[string]string{"b.ts": "", "a.ts": ""})
	assert.Equal(t, header+"export * from './a.js';\nexport * from './b.js';\n", got)
}

func TestAddFilesRejectsCollisions(t *testing.T) {
	t.Parallel()

	files := map[string]string{"tools.ts": "types"}
	require.ErrorContains(t, addFiles(files, map[string]string{"tools.ts": "zod"}), "two emitters write tools.ts")
	require.NoError(t, addFiles(files, map[string]string{"toolArgs.ts": "zod"}))
}

func TestBuildFailsLoudly(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		edit func(*config)
		want string
	}{
		"custom encoding without override": {
			edit: func(c *config) { c.overrides = nil },
			want: "implements json.Marshaler",
		},
		"package without module": {
			edit: func(c *config) { delete(c.modules, samplePkg) },
			want: "has no output module",
		},
		"name collision": {
			edit: func(c *config) { c.names = map[reflect.Type]string{reflect.TypeFor[SampleItem](): "SampleKind"} },
			want: "are both named SampleKind",
		},
		"shadowed global": {
			edit: func(c *config) { c.names = map[reflect.Type]string{reflect.TypeFor[SampleItem](): "Error"} },
			want: "shadows a TypeScript global",
		},
		"enum without constants": {
			edit: func(c *config) {
				c.enums[reflect.TypeFor[SampleOpen]()] = nil
				c.roots = []reflect.Type{reflect.TypeFor[SampleOpen]()}
			},
			want: "no string constants",
		},
		"non-string enum": {
			edit: func(c *config) {
				c.modules["time"] = "sample"
				c.enums[reflect.TypeFor[time.Month]()] = nil
				c.roots = []reflect.Type{reflect.TypeFor[time.Month]()}
			},
			want: "only string enums",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := sampleConfig()
			tt.edit(cfg)
			_, err := sampleBuild(t, cfg)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestEmitTypeScriptRejectsUnsupportedSchemas(t *testing.T) {
	t.Parallel()

	_, err := emitTypeScript([]*def{{name: "X", module: "x", schema: &jsonschema.Schema{AllOf: []*jsonschema.Schema{{Type: "string"}}}}})
	require.ErrorIs(t, err, errUnsupportedSchema)
}
