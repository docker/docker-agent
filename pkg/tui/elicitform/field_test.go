package elicitform

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefaultAnswer(t *testing.T) {
	t.Parallel()

	assert.Equal(t, Answer{Bool: true}, DefaultAnswer(Field{Type: TypeBoolean, Default: true}))
	assert.Equal(t, Answer{Choice: 1}, DefaultAnswer(Field{Type: TypeEnum, EnumValues: []string{"a", "b"}, Default: "b"}))
	assert.Equal(t, Answer{}, DefaultAnswer(Field{Type: TypeEnum, EnumValues: []string{"a"}, Default: "zzz"}))
	assert.Equal(t, Answer{Checked: []bool{false, true}}, DefaultAnswer(Field{Type: TypeMultiSelect, EnumValues: []string{"a", "b"}, Default: []any{"b"}}))
	assert.Equal(t, Answer{Text: "42"}, DefaultAnswer(Field{Type: TypeInteger, Default: 42}))
	assert.Equal(t, Answer{}, DefaultAnswer(Field{Type: TypeString}))
}

func TestCollect(t *testing.T) {
	t.Parallel()

	fields := []Field{
		{Name: "name", Type: TypeString, Required: true},
		{Name: "age", Type: TypeInteger, HasMinimum: true, Minimum: 0},
		{Name: "secret", Type: TypeString, Format: "password"},
		{Name: "ok", Type: TypeBoolean},
		{Name: "color", Type: TypeEnum, EnumValues: []string{"red", "blue"}},
		{Name: "pets", Type: TypeMultiSelect, EnumValues: []string{"cat", "dog"}, MinItems: 1},
	}

	t.Run("valid answers", func(t *testing.T) {
		t.Parallel()
		answers := []Answer{
			{Text: "  Ada "},
			{Text: "36"},
			{Text: " pw "},
			{Bool: true},
			{Choice: 1},
			{Checked: []bool{true, false}},
		}
		content, errs, first := Collect(fields, func(i int) Answer { return answers[i] })
		assert.Equal(t, -1, first)
		assert.Empty(t, errs)
		assert.Equal(t, map[string]any{
			"name":   "Ada",
			"age":    int64(36),
			"secret": " pw ",
			"ok":     true,
			"color":  "blue",
			"pets":   []any{"cat"},
		}, content)
	})

	t.Run("reports every invalid field", func(t *testing.T) {
		t.Parallel()
		answers := []Answer{
			{},
			{Text: "-1"},
			{},
			{},
			{Choice: 0},
			{Checked: []bool{false, false}},
		}
		content, errs, first := Collect(fields, func(i int) Answer { return answers[i] })
		assert.Equal(t, 0, first)
		assert.Equal(t, map[int]string{
			0: "This field is required",
			1: "Must be at least 0",
			5: "Select at least 1",
		}, errs)
		assert.NotContains(t, content, "secret")
	})
}

func TestFreeForm(t *testing.T) {
	t.Parallel()

	assert.Nil(t, FreeForm("   "))
	assert.Equal(t, map[string]any{"response": "yes"}, FreeForm(" yes "))
}
