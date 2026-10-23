package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/integrations/dialguard"
	"vornik.io/vornik/internal/registry"
)

// GitHub #494 item 5 (2026-10-21, integrations-hub-design "Amendment
// 2026-10-21 (#494)", the census and change item 3): three production
// callers of the library fallbacks used a client with no dial guard. These
// pin that each now dials through the shared guard.
//
// The private target is 0.0.0.0:port. Linux delivers 0.0.0.0 to the local
// host, so an unguarded client CONNECTS to the counting listener below; the
// guard refuses 0.0.0.0/8 before connect(2). A connection count of zero is
// the proof that no packet left for the private address.

// countingListener accepts TCP connections on 127.0.0.1 and counts them; it
// answers nothing (each connection is closed at once).
func countingListener(t *testing.T) (port string, conns *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	conns = &atomic.Int32{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			_ = c.Close()
		}
	}()
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	return port, conns
}

func getVia(client *http.Client, rawURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

// The connector's clients for an operator project's MCP OAuth flow. Before
// #494 the per-server function returned nil for an operator project, and the
// connector fell back to a plain &http.Client{Timeout: 30s}. The server's own
// metadata names the token and registration endpoints, so a public MCP server
// could point the daemon's code exchange at a private address.
//
// Lower than the found-at level (review; an attacker would reach it through a
// malicious server's OAuth metadata during Connect) because the connector
// flow needs a full OAuth round trip; these are the exact clients the
// production connector is built with (mcpOAuthClients).
func TestMCPOAuthHTTP_OperatorProjectIsGuarded_Issue494(t *testing.T) {
	port, conns := countingListener(t)
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
	fallback, perServer := c.mcpOAuthClients()

	target := "http://0.0.0.0:" + port + "/token"
	clients := map[string]*http.Client{"fallback": fallback}
	if h := perServer("ops-project", "https://mcp.example.com/mcp"); h != nil {
		clients["operator project"] = h
	}
	if h := perServer("ops-project", "https://mcp.example.com/mcp"); h == nil {
		t.Error("an operator project gets no per-server client, so the connector's fallback decides")
	}
	for name, client := range clients {
		err := getVia(client, target)
		if !errors.Is(err, dialguard.ErrDialRefused) {
			t.Errorf("%s client: dial of %s returned %v, want ErrDialRefused", name, target, err)
		}
	}
	if n := conns.Load(); n != 0 {
		t.Fatalf("the private address received %d connection(s) from the MCP OAuth clients", n)
	}

	// The operator's integrations.allowed_hosts vouches for a host: a
	// loopback server URL is dialled when it resolves to loopback.
	local := perServer("ops-project", "http://127.0.0.1:"+port+"/mcp")
	if local == nil {
		t.Fatal("no per-server client for a loopback operator server")
	}
	_ = getVia(local, "http://127.0.0.1:"+port+"/mcp")
	if conns.Load() == 0 {
		t.Error("the operator's loopback MCP server URL was refused")
	}
}

// The GitHub App channel's client. Before #494 resolveGitHubAppConfig left
// Config.HTTPClient nil and github.New fell back to http.DefaultClient, so
// api_base_url (a GitHub Enterprise base may name any address) was dialled
// unguarded. Driven through the production builder and the channel's Start,
// which dials the installation-permission check at once.
func TestGitHubAppChannel_APIBaseURLIsGuarded_Issue494(t *testing.T) {
	port, conns := countingListener(t)
	t.Setenv("GH_TEST_SECRET", "shhh")
	pemPath := writePEM(t, t.TempDir()+"/key.pem")
	p := inboundOnlyProject("p-494")
	p.GitHubApp.AppID = 12345
	p.GitHubApp.InstallationID = 99
	p.GitHubApp.PrivateKeyPath = pemPath
	p.GitHubApp.APIBaseURL = "http://0.0.0.0:" + port
	ch, _, err := buildGitHubChannelWithAllowedHosts([]*registry.Project{p}, nil, nil, zerolog.Nop())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Start runs the permission check synchronously, then waits for ctx: when
	// it returns, the check has run.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = ch.Start(ctx, nil)
	if n := conns.Load(); n != 0 {
		t.Fatalf("api_base_url on a private address received %d connection(s); the GitHub client must dial through the guard", n)
	}
}

// The Slack channel's client. Before #494 buildSlackChannelForProject set
// http.DefaultClient. FetchAttachment downloads a URL a Slack event names
// (the check is https only), with the bot token attached.
func TestSlackChannel_AttachmentFetchIsGuarded_Issue494(t *testing.T) {
	port, conns := countingListener(t)
	p := projectWithSlack("proj-494", "T494", "SLACK_SIGN_494", "SLACK_BOT_494")
	t.Setenv("SLACK_SIGN_494", "sign")
	t.Setenv("SLACK_BOT_494", "xoxb-test")
	ch, err := buildSlackChannelForProject(p, nil, nil, zerolog.Nop())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ref := conversation.Attachment{ChannelRef: "slack:T494:https://0.0.0.0:" + port + "/files/x.png"}
	_, err = ch.FetchAttachment(context.Background(), ref)
	if err == nil || !strings.Contains(err.Error(), "dial guard") {
		t.Errorf("attachment fetch from a private address: err = %v, want the dial guard's refusal", err)
	}
	if n := conns.Load(); n != 0 {
		t.Fatalf("the private address received %d connection(s) from the Slack client", n)
	}
}
