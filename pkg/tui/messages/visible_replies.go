package messages

// VisibleReplies controls how many regular assistant replies remain visible
// in the transcript. Older replies of an exchange fold into its Work block.
type VisibleReplies string

const (
	VisibleRepliesLatest VisibleReplies = "latest"
	VisibleRepliesThree  VisibleReplies = "3"
	VisibleRepliesFive   VisibleReplies = "5"
	VisibleRepliesAll    VisibleReplies = "all"
)

// ParseVisibleReplies normalizes configured values, defaulting to the latest
// three replies.
func ParseVisibleReplies(raw string) VisibleReplies {
	switch VisibleReplies(raw) {
	case VisibleRepliesLatest, VisibleRepliesThree, VisibleRepliesFive, VisibleRepliesAll:
		return VisibleReplies(raw)
	default:
		return VisibleRepliesThree
	}
}
