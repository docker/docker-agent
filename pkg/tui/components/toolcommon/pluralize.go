package toolcommon

import "fmt"

// Pluralize formats a count with its singular or plural form,
// e.g. Pluralize(1, "file", "files") returns "1 file" while
// Pluralize(3, "file", "files") returns "3 files".
func Pluralize(count int, singular, plural string) string {
	word := plural
	if count == 1 {
		word = singular
	}
	return fmt.Sprintf("%d %s", count, word)
}
