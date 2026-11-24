package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLazySource(t *testing.T) {
	t.Parallel()
	const src = `package anthropic

type ExampleUnion struct{ paramUnion }

func init() {
	apijson.RegisterUnion[ExampleUnion](
		"kind",
		apijson.Discriminator[First]("one"),
		apijson.Discriminator[Second]("two"),
	)
}
`
	out, err := lazySource("example.go", []byte(src))
	require.NoError(t, err)
	assert.Equal(t, `package anthropic

type ExampleUnion struct{ paramUnion }

func (*ExampleUnion) APIJSONUnion() (string, []apijson.UnionVariant) {
	return "kind", []apijson.UnionVariant{
		apijson.Discriminator[First]("one"),
		apijson.Discriminator[Second]("two"),
	}
}
`, string(out))
}

func TestLazySourceRejectsUnexpectedInit(t *testing.T) {
	t.Parallel()
	_, err := lazySource("example.go", []byte(`package anthropic
func init() { setup() }
`))
	require.ErrorContains(t, err, "expected a generated request union registration")
}
