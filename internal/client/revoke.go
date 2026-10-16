package client

import (
	"context"
	"net/http"
	"strings"
)

// Revoke uses only the device credential and confers no resident authority.
// The endpoint is idempotent for the same current credential after revocation.
func (c *Client) Revoke(ctx context.Context, state State) error {
	if validateURL(state.URL) != nil {
		return ErrTransport
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(state.URL, "/")+"/api/v1/companion/revoke", nil)
	if err != nil {
		return ErrTransport
	}
	req.Header.Set("Authorization", "Bearer "+state.Credential)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return ErrTransport
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		return ErrAuthority
	}
	return nil
}
