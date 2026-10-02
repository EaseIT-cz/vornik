// Package agentapi is the REST client of agent-namespace projects
// (agent-administered Vornik design §8.3; plan P4.5): an apigateway.Client
// that calls a project's own approved APIs directly, injecting the
// credential from the namespaced secret store at call time. Operator
// projects keep the Kong-backed client.
//
// It refuses before sending anything: a method outside the construction's
// AllowedMethods, a provider without a live api approval, a method outside
// the approved READ set (the role route) or WRITE set (the worker route),
// and a path that could leave the base URL. It dials through the caller's
// SSRF-guarded client, follows no cross-host redirect, reads at most
// MaxBodyBytes, scrubs the credential from the body, and logs method, host,
// status and byte counts only.
package agentapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// MaxBodyBytes caps a response body.
const MaxBodyBytes = 1 << 20

// SecretSource resolves a namespaced secret ("<ns>/<NAME>").
type SecretSource interface {
	Get(name string) (string, bool)
}

// Client calls one agent project's APIs.
type Client struct {
	ProjectID string
	Project   func(id string) *registry.Project
	Grants    persistence.AgentGrantRepository
	Secrets   SecretSource
	// AllowedMethods is the construction's method allowlist: {GET, HEAD} on
	// the role route, the action's one method on the worker route.
	AllowedMethods map[string]bool
	// Write judges the method against the approved WRITE set (the worker);
	// otherwise against the READ set.
	Write bool
	// HTTP returns the guarded client for a base URL.
	HTTP   func(baseURL string) *http.Client
	Logger zerolog.Logger
}

var (
	_ apigateway.Client         = (*Client)(nil)
	_ apigateway.ProviderLister = (*Client)(nil)
)

// ListProviders lists the project's APIs whose approval is live, with the
// methods this route may use (list_apis).
func (c *Client) ListProviders() []apigateway.ProviderInfo {
	p := c.Project(c.ProjectID)
	if p == nil || c.Grants == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out []apigateway.ProviderInfo
	for _, api := range p.APIs {
		a, err := c.Grants.GetIntegration(ctx, c.ProjectID, api.Name)
		if err != nil || a.RemovedAt != nil || a.Kind != "api" {
			continue
		}
		set := a.ReadTools
		if c.Write {
			set = a.WriteTools
		}
		var methods []string
		for _, m := range set {
			if c.AllowedMethods[m] && contains(api.Methods, m) {
				methods = append(methods, m)
			}
		}
		out = append(out, apigateway.ProviderInfo{Name: api.Name, Description: "your API at " + hostOfURL(api.BaseURL), AllowedMethods: methods})
	}
	return out
}

func hostOfURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// Call makes one request.
func (c *Client) Call(ctx context.Context, req apigateway.Request) (apigateway.Response, error) {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	if !c.AllowedMethods[method] {
		return apigateway.Response{}, apigateway.ErrMethodNotAllowed
	}
	api, err := c.approvedAPI(ctx, req.Provider, method)
	if err != nil {
		return apigateway.Response{}, err
	}
	target, err := joinPath(api.BaseURL, req.Path, req.Query)
	if err != nil {
		return apigateway.Response{}, err
	}
	httpReq, secret, err := c.build(ctx, api, method, target, req.Body)
	if err != nil {
		return apigateway.Response{}, err
	}
	return c.do(httpReq, api, secret)
}

// approvedAPI returns the project's API when its approval is live and admits
// the method on this route.
func (c *Client) approvedAPI(ctx context.Context, provider, method string) (registry.ProjectAPI, error) {
	var api *registry.ProjectAPI
	if p := c.Project(c.ProjectID); p != nil {
		for i := range p.APIs {
			if p.APIs[i].Name == provider {
				api = &p.APIs[i]
			}
		}
	}
	if api == nil || c.Grants == nil {
		return registry.ProjectAPI{}, apigateway.ErrUnknownProvider
	}
	a, err := c.Grants.GetIntegration(ctx, c.ProjectID, provider)
	if err != nil || a.RemovedAt != nil || a.Kind != "api" {
		return registry.ProjectAPI{}, apigateway.ErrUnknownProvider
	}
	set := a.ReadTools
	if c.Write {
		set = a.WriteTools
	}
	if !contains(set, method) || !contains(api.Methods, method) {
		return registry.ProjectAPI{}, apigateway.ErrMethodNotAllowed
	}
	return *api, nil
}

// joinPath builds base + path + query and refuses anything that could
// leave the base. The path is decoded ONCE; a value that still decodes
// further (a double encoding an upstream may decode again) is refused, as
// are dot and empty segments, a backslash, NUL or path parameter (';'),
// and an absolute or
// protocol-relative URL. The request path is then rebuilt from the decoded
// segments, each escaped once, so what is sent is exactly what was checked,
// and it must stay under the base path (review 20261002-2d4f F1).
func joinPath(base, rawPath string, query map[string]any) (*url.URL, error) {
	trimmed := strings.TrimSpace(rawPath)
	if strings.Contains(trimmed, "://") || strings.HasPrefix(trimmed, "//") {
		return nil, apigateway.ErrInvalidPath
	}
	decoded, err := url.PathUnescape(trimmed)
	if err != nil {
		return nil, apigateway.ErrInvalidPath
	}
	if again, err := url.PathUnescape(decoded); err != nil || again != decoded {
		return nil, apigateway.ErrInvalidPath // double-encoded
	}
	// A ';' starts a path parameter, which some upstreams strip after this
	// check (Tomcat-style "..;x" -> "..") (review 20261002-c65a F1). This
	// deliberately refuses every ';', a legitimate one included: no
	// configured API uses matrix parameters, and one that needs them should
	// trip this consciously (review 20261002-2acb F3).
	if strings.ContainsAny(decoded, "\\\x00?#;") {
		return nil, apigateway.ErrInvalidPath
	}
	segs := strings.Split(strings.Trim(decoded, "/"), "/")
	escaped := make([]string, 0, len(segs))
	for _, seg := range segs {
		if seg == "" && decoded != "" && decoded != "/" {
			return nil, apigateway.ErrInvalidPath // a//b
		}
		if seg == "." || seg == ".." {
			return nil, apigateway.ErrInvalidPath
		}
		if seg != "" {
			escaped = append(escaped, url.PathEscape(seg))
		}
	}
	b, err := url.Parse(base)
	if err != nil {
		return nil, apigateway.ErrInvalidPath
	}
	basePath := strings.TrimRight(b.EscapedPath(), "/")
	u := *b
	u.RawPath = basePath + "/" + strings.Join(escaped, "/")
	u.Path, err = url.PathUnescape(u.RawPath)
	if err != nil || !strings.HasPrefix(u.EscapedPath(), basePath+"/") {
		return nil, apigateway.ErrInvalidPath
	}
	if len(query) > 0 {
		q := url.Values{}
		for k, v := range query {
			q.Set(k, fmt.Sprintf("%v", v))
		}
		u.RawQuery = q.Encode()
	}
	return &u, nil
}

func (c *Client) build(ctx context.Context, api registry.ProjectAPI, method string, target *url.URL, body map[string]any) (*http.Request, string, error) {
	var rdr io.Reader
	if body != nil && !registry.APIReadMethods[method] {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, "", fmt.Errorf("%w: the body is not JSON", apigateway.ErrGatewayRequest)
		}
		rdr = bytes.NewReader(raw)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, target.String(), rdr)
	if err != nil {
		return nil, "", apigateway.ErrGatewayRequest
	}
	httpReq.Header.Set("Accept", "application/json")
	if rdr != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	secret := ""
	if ref := strings.TrimSpace(api.Auth.ValueFrom); ref != "" {
		v, ok := c.Secrets.Get(strings.TrimPrefix(ref, "secret://"))
		if !ok || v == "" {
			return nil, "", fmt.Errorf("%w: the API's credential is not set yet; ask the user to add it", apigateway.ErrGatewayAuth)
		}
		header := api.Auth.Header
		if header == "" {
			header = "Authorization"
		}
		httpReq.Header.Set(header, api.Auth.Prefix+v)
		secret = v
	}
	return httpReq, secret, nil
}

func (c *Client) do(httpReq *http.Request, api registry.ProjectAPI, secret string) (apigateway.Response, error) {
	hc := *c.HTTP(api.BaseURL)
	host := httpReq.URL.Hostname()
	hc.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if r.URL.Hostname() != host || len(via) >= 3 {
			return http.ErrUseLastResponse
		}
		return nil
	}
	resp, err := hc.Do(httpReq)
	if err != nil {
		c.Logger.Warn().Str("api", api.Name).Str("method", httpReq.Method).Str("host", host).Msg("agent api: request failed")
		return apigateway.Response{}, fmt.Errorf("%w: the request did not complete", apigateway.ErrGatewayRequest)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if len(raw) > MaxBodyBytes {
		raw = append(raw[:MaxBodyBytes], []byte("\n[truncated]")...)
	}
	body := scrub(string(raw), secret, api.Auth.Prefix)
	c.Logger.Info().Str("api", api.Name).Str("method", httpReq.Method).Str("host", host).
		Int("status", resp.StatusCode).Int("bytes", len(raw)).Msg("agent api: call")
	return apigateway.Response{Status: resp.StatusCode, Body: body}, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// scrub removes the credential from a response body: raw, and in the
// encodings an upstream might echo it in - URL-encoded and base64 (standard
// and URL alphabets, padded or not), with and without its prefix (review
// 20261002-2d4f F5). An echo in any other transformation is not caught.
func scrub(body, secret, prefix string) string {
	if secret == "" {
		return body
	}
	var forms []string
	for _, v := range []string{prefix + secret, secret} {
		forms = append(forms, v, url.QueryEscape(v), url.PathEscape(v),
			base64.StdEncoding.EncodeToString([]byte(v)), base64.RawStdEncoding.EncodeToString([]byte(v)),
			base64.URLEncoding.EncodeToString([]byte(v)), base64.RawURLEncoding.EncodeToString([]byte(v)))
	}
	for _, f := range forms {
		if f != "" {
			body = strings.ReplaceAll(body, f, "[REDACTED]")
		}
	}
	return body
}
