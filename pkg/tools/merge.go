package tools

// Mergeable is implemented by toolsets that must be combined with sibling
// toolsets sharing their MergeKey when an agent declares several of them,
// instead of being exposed separately. The LSP toolset uses this so the model
// sees one set of lsp_* tools routing by file type rather than duplicates.
// Loaders call Merge on the first sibling with all of them, itself included,
// in declaration order.
type Mergeable interface {
	ToolSet
	MergeKey() string
	Merge(siblings []MergeSibling) ToolSet
}

// MergeSibling pairs a raw toolset with the wrapped form the loader built
// from its configuration (tool filters, instructions, ...).
type MergeSibling struct {
	Raw     ToolSet
	Wrapped ToolSet
}
