package github

// GitHub #802 (github-app-channel design, "One-click App creation",
// 2026-11-18): the App-manifest flow. BuildManifest is what the console
// posts to GitHub; ConvertManifestCode exchanges the code GitHub redirects
// back with for the App's id, key and webhook secret.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// manifestResponseCap bounds the conversion's body: a PEM is about
	// 1.7 KiB and the whole answer well under 16 KiB.
	manifestResponseCap = 256 << 10
	// manifestConvertTimeout bounds the conversion call.
	manifestConvertTimeout = 15 * time.Second
)

var (
	// manifestCodeRE: GitHub's code is a short token; anything else could
	// change the request path.
	manifestCodeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)
	// githubOwnerRE is GitHub's login rule: alphanumerics and single
	// hyphens, not leading.
	githubOwnerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
)

// httpsOrigin returns scheme://host of raw, refusing anything that is not
// a bare https origin.
func httpsOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return nil, fmt.Errorf("github: %q is not an https origin (scheme://host[:port])", raw)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

// BuildManifest renders the manifest for an App served at origin. Its
// hook_attributes carry only url and active: the manifest cannot carry a
// webhook secret, and GitHub generates one.
func BuildManifest(origin string) ([]byte, error) {
	o, err := httpsOrigin(origin)
	if err != nil {
		return nil, err
	}
	base := o.String()
	return json.Marshal(map[string]any{
		"name": "Vornik (" + o.Hostname() + ")",
		"url":  base,
		"hook_attributes": map[string]any{
			"url":    base + "/api/v1/github-app/webhook",
			"active": true,
		},
		"redirect_url":    base + "/ui/github/callback",
		"setup_url":       base + "/ui/github/installed",
		"setup_on_update": true,
		"public":          false,
		"default_permissions": map[string]string{
			"contents":      "write",
			"issues":        "write",
			"pull_requests": "write",
			"checks":        "write",
			"metadata":      "read",
		},
		"default_events": []string{"issues", "issue_comment", "pull_request", "pull_request_review_comment"},
	})
}

// ManifestEndpoints returns the form action the console posts the manifest
// to, and the REST base the conversion calls. webBase is "" for github.com
// or a GitHub Enterprise origin; org is "" for the signed-in account.
func ManifestEndpoints(webBase, org, state string) (action, apiBase string, err error) {
	web, api := "https://github.com", "https://api.github.com"
	if strings.TrimSpace(webBase) != "" {
		o, err := httpsOrigin(webBase)
		if err != nil {
			return "", "", err
		}
		web, api = o.String(), o.String()+"/api/v3"
	}
	path := "/settings/apps/new"
	if org != "" {
		if !githubOwnerRE.MatchString(org) {
			return "", "", fmt.Errorf("github: %q is not a GitHub organisation name", org)
		}
		path = "/organizations/" + org + path
	}
	return web + path + "?state=" + url.QueryEscape(state), api, nil
}

// ManifestApp is what Vornik keeps from a conversion. The OAuth client
// secret is not decoded at all: Vornik signs no user in with the App.
type ManifestApp struct {
	ID            int64
	Slug          string
	Name          string
	Owner         string
	HTMLURL       string
	PEM           string
	WebhookSecret string
}

// ConvertManifestCode calls POST <apiBase>/app-manifests/<code>/conversions,
// unauthenticated as GitHub specifies, through client (the caller's
// dial-guarded client), bounded by manifestConvertTimeout and
// manifestResponseCap. An error never carries the body's secrets: only the
// status and, for a refusal, GitHub's message.
func ConvertManifestCode(ctx context.Context, client *http.Client, apiBase, code string) (*ManifestApp, error) {
	if !manifestCodeRE.MatchString(code) {
		return nil, errors.New("github: the code GitHub returned is malformed")
	}
	ctx, cancel := context.WithTimeout(ctx, manifestConvertTimeout)
	defer cancel()
	endpoint := strings.TrimRight(apiBase, "/") + "/app-manifests/" + code + "/conversions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(nil))
	if err != nil {
		return nil, fmt.Errorf("github: conversion request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: conversion call failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, manifestResponseCap+1))
	if err != nil {
		return nil, fmt.Errorf("github: reading the conversion: %w", err)
	}
	if len(body) > manifestResponseCap {
		return nil, errors.New("github: the conversion answer is larger than expected")
	}
	if resp.StatusCode != http.StatusCreated {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		msg := e.Message
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("github: the conversion answered %d: %s", resp.StatusCode, msg)
	}
	var out struct {
		ID      int64  `json:"id"`
		Slug    string `json:"slug"`
		Name    string `json:"name"`
		HTMLURL string `json:"html_url"`
		Owner   struct {
			Login string `json:"login"`
		} `json:"owner"`
		PEM           string `json:"pem"`
		WebhookSecret string `json:"webhook_secret"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errors.New("github: the conversion answer is not JSON")
	}
	if out.ID <= 0 || out.Slug == "" || out.PEM == "" || out.WebhookSecret == "" {
		return nil, errors.New("github: the conversion answer lacks the App id, slug, key or webhook secret")
	}
	return &ManifestApp{ID: out.ID, Slug: out.Slug, Name: out.Name, Owner: out.Owner.Login,
		HTMLURL: out.HTMLURL, PEM: out.PEM, WebhookSecret: out.WebhookSecret}, nil
}
