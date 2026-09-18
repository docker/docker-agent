package tools

import (
	"context"
	"slices"
)

// Catalog is implemented by toolsets holding tools the model reaches through
// provider-native tool search (e.g. the deferred toolset's deferred tools).
// CatalogTools returns every catalog tool, handler included, whether or not
// the toolset currently also lists it through Tools() (e.g. after add_tool):
// the provider owns discovery, so the declaration must not change with
// activation state. The host marks catalog tools [Tool.InCatalog], those no
// toolset lists also [Tool.SearchOnly], and keeps a same-named regular tool
// from another toolset instead.
type Catalog interface {
	CatalogTools(ctx context.Context) ([]Tool, error)
}

// WithoutSearchOnly returns requestTools without [Tool.SearchOnly] entries,
// for providers that cannot expose them through tool search. Listed
// [Tool.InCatalog] tools stay: they are regular tools for such providers. The
// input is returned as-is when nothing needs dropping.
func WithoutSearchOnly(requestTools []Tool) []Tool {
	if !slices.ContainsFunc(requestTools, func(t Tool) bool { return t.SearchOnly }) {
		return requestTools
	}
	return slices.DeleteFunc(slices.Clone(requestTools), func(t Tool) bool { return t.SearchOnly })
}
