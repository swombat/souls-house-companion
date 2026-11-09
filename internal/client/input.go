package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func validDeadline(r Request, now time.Time) bool {
	maximum := 31 * time.Second
	switch r.Operation {
	case "execute":
		maximum = 301 * time.Second
	case "desktop":
		maximum = 121 * time.Second
	}
	return r.Deadline.After(now) && !r.Deadline.After(now.Add(maximum))
}

// Input downloads are consumed once. An ambiguous failure is never retried.
func (c *Client) input(ctx context.Context, state State, r Request) ([]byte, error) {
	if validateURL(state.URL) != nil || (r.Operation != "write" && r.Operation != "execute" && r.Operation != "desktop") {
		return nil, ErrAuthority
	}
	var args struct {
		Size *int `json:"input_size"`
	}
	if json.Unmarshal(r.Arguments, &args) != nil || args.Size == nil || *args.Size < 0 || *args.Size > 8<<20 {
		return nil, ErrTransport
	}
	if c.BeforeDelivery != nil && c.BeforeDelivery() != nil {
		return nil, ErrAuthority
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(state.URL, "/")+"/api/v1/companion/operations/"+url.PathEscape(r.ID)+"/input", nil)
	if err != nil {
		return nil, ErrTransport
	}
	req.Header.Set("Authorization", "Bearer "+state.Credential)
	req.Header.Set("X-Companion-Epoch", r.Epoch)
	req.Header.Set("X-Companion-Revision", strconv.Itoa(r.Revision))
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, ErrTransport
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, ErrTransport
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, int64(*args.Size)+1))
	if err != nil || len(data) != *args.Size || ctx.Err() != nil {
		return nil, ErrTransport
	}
	if c.BeforeDelivery != nil && c.BeforeDelivery() != nil {
		return nil, ErrAuthority
	}
	return data, nil
}
