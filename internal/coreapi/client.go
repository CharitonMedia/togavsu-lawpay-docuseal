// Package coreapi is the module's typed client for the core API calls in SPEC section 6.
// Every call carries the module token. An interactive call adds the viewer's context
// token; a background write adds a core-issued task authorization; a call with neither is
// a service call (CORE-01), which the core runs as the module's service principal.
package coreapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Headers the core reads.
const (
	ContextHeader = "X-Togavsu-Context-Token"
	TaskHeader    = "X-Togavsu-Task-Authorization"
)

// Client calls the core.
type Client struct {
	Base, Token, Module string
	HTTP                *http.Client
}

// Auth is how one call authenticates beyond the module token.
type Auth struct {
	// Context is the viewer's context token (a viewer call).
	Context string
	// Task is a core-issued task authorization (a granted background write).
	Task string
	// Idempotency is the Idempotency-Key, for writes the module may retry.
	Idempotency string
}

// Service is a service call: no viewer and no grant.
var Service = Auth{}

// Viewer is a viewer call.
func Viewer(token string) Auth { return Auth{Context: token} }

// Error is a non-2xx core answer: the problem body plus the status.
type Error struct {
	Status    int    `json:"-"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Field     string `json:"field,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("core %d %s: %s", e.Status, e.Code, e.Message)
}

// Is reports whether err is a core answer with status.
func Is(err error, status int) bool {
	var ce *Error
	return errors.As(err, &ce) && ce.Status == status
}

// HasCode reports whether err is a core answer with code (as code, error code or in the
// message, since the problem body names a contract code in any of the three).
func HasCode(err error, code string) bool {
	var ce *Error
	if !errors.As(err, &ce) {
		return false
	}
	return ce.Code == code || ce.ErrorCode == code || strings.Contains(ce.Message, code)
}

// Do calls method path with body, decoding a 2xx answer into out.
func (c *Client) Do(ctx context.Context, method, path string, a Auth, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	return c.DoRaw(ctx, method, path, a, rd, "application/json", nil, out)
}

// DoRaw sends a raw body with contentType and extra headers.
func (c *Client) DoRaw(ctx context.Context, method, path string, a Auth, body io.Reader, contentType string, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Base, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if a.Context != "" {
		req.Header.Set(ContextHeader, a.Context)
	}
	if a.Task != "" {
		req.Header.Set(TaskHeader, a.Task)
	}
	if a.Idempotency != "" {
		req.Header.Set("Idempotency-Key", a.Idempotency)
	}
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		ce := &Error{Status: resp.StatusCode}
		if json.Unmarshal(raw, ce) != nil || ce.Code == "" {
			ce.Code, ce.Message = http.StatusText(resp.StatusCode), strings.TrimSpace(string(raw))
		}
		ce.Status = resp.StatusCode
		return ce
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Path joins escaped segments onto a path prefix.
func Path(prefix string, segments ...string) string {
	var b strings.Builder
	b.WriteString(prefix)
	for _, s := range segments {
		b.WriteString("/")
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}
