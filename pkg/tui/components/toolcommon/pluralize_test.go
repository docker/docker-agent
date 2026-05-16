package toolcommon

import "testing"

func TestPluralize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		count    int
		singular string
		plural   string
		expected string
	}{
		{0, "file", "files", "0 files"},
		{1, "file", "files", "1 file"},
		{2, "file", "files", "2 files"},
		{100, "file", "files", "100 files"},
		{1, "directory", "directories", "1 directory"},
		{2, "directory", "directories", "2 directories"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			t.Parallel()
			got := Pluralize(tt.count, tt.singular, tt.plural)
			if got != tt.expected {
				t.Errorf("Pluralize(%d, %q, %q) = %q, want %q",
					tt.count, tt.singular, tt.plural, got, tt.expected)
			}
		})
	}
}
