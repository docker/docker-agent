package attachment

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// fetchTimeout is the maximum time allowed for a URL fetch.
const fetchTimeout = 10 * time.Second

// FetchURL fetches a public URL with a 10-second timeout.
// Returns the raw response body bytes on success.
// Returns an error if the request fails or the server responds with a non-2xx status.
func FetchURL(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("attachment: creating request for %q: %w", url, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("attachment: fetching %q: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("attachment: fetching %q: unexpected status %d", url, resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("attachment: reading response from %q: %w", url, err)
	}

	return data, nil
}
