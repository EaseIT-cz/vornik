package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/config"
)

// Installation credential hygiene (GitHub #412). The reference host ran its
// production database on the integration lane's credential pair, listening on
// 0.0.0.0:5432, and no doctor check read the password's value, the listener or
// whether one secret served two purposes. These three checks do.
// see LLD § https://docs.vornik.io §2.1 D5
//
// Every message names config PATHS and the rule that matched, never a value:
// when the password equals a known default or the role name, printing either
// would print the live credential.

// dbCredentialSnapshot is what the database checks read, captured once in
// SetServerConfig like the other security checks' inputs.
type dbCredentialSnapshot struct {
	known    bool
	driver   string
	host     string
	port     int
	user     string
	name     string
	password string
}

// secretRef is one secret-bearing config field, for credential_reuse.
type secretRef struct {
	path    string
	purpose string
	value   string
}

// knownDefaultDBPasswords are the shipped constants: values an installer, an
// example or a test lane has shipped, plus the generic guesses. Compared
// case-insensitively; every entry is lower-case. (The strength check also
// flags a password equal to the role or the database name; that is separate.)
// This is the source of truth for the quickstart's known_default_db_password
// (deployments/podman/quickstart.sh), which must list exactly these values:
// TestDoctor412_QuickstartKnownDefaultsMatchDoctor fails when they differ.
var knownDefaultDBPasswords = map[string]bool{
	"vornik": true, "swarmd": true, "postgres": true, "vornik_dev_password": true,
	"password": true, "changeme": true, "secret": true, "admin": true, "test": true,
	"your-secure-password": true,
}

// testLaneDBNames are the integration lane's default role/password values
// (scripts/integration-db-lane.sh, CI, and the reference host's lane).
var testLaneDBNames = map[string]bool{"vornik": true, "swarmd": true}

// minDBPasswordLen is the WARNING floor for an operator-chosen password. Both
// installers generate far more (quickstart 64 hex, gen-config ~32 base64url).
const minDBPasswordLen = 16

func (h *DoctorHandlers) snapshotDBCredentials(cfg *config.Config) {
	h.dbCreds = dbCredentialSnapshot{
		known:    true,
		driver:   cfg.Database.Driver,
		host:     cfg.Database.Host,
		port:     cfg.Database.Port,
		user:     cfg.Database.User,
		name:     cfg.Database.Name,
		password: cfg.Database.Password,
	}
	refs := []secretRef{{"database.password", "database", cfg.Database.Password}}
	for i, k := range cfg.API.APIKeys {
		refs = append(refs, secretRef{fmt.Sprintf("api.api_keys[%d]", i), "operator API", k})
	}
	for i, k := range cfg.Admin.AllowedKeys {
		refs = append(refs, secretRef{fmt.Sprintf("admin.allowed_keys[%d]", i), "operator API", k})
	}
	refs = append(refs,
		secretRef{"chat.api_key", "LLM provider", cfg.Chat.APIKey},
		secretRef{"chat.router.http.api_key", "LLM provider", cfg.Chat.Router.HTTP.APIKey},
		secretRef{"runtime.agent_llm.api_key", "LLM provider", cfg.Runtime.AgentLLM.APIKey},
		secretRef{"memory.embedding_api_key", "LLM provider", cfg.Memory.EmbeddingAPIKey},
		secretRef{"telegram.bot_token", "Telegram", cfg.Telegram.BotToken},
		secretRef{"auth.providers.github.client_secret", "GitHub OAuth", githubClientSecret(cfg)},
		secretRef{"storage.s3.secret_access_key", "artifact store", cfg.Storage.S3.SecretAccessKey},
	)
	if s, err := cfg.Web.ResolvedSubmitSecret(); err == nil {
		refs = append(refs, secretRef{"web.submit_secret", "scraper submit", s})
	}
	peers := make([]string, 0, len(cfg.A2A.Peers))
	for k := range cfg.A2A.Peers {
		peers = append(peers, k)
	}
	sort.Strings(peers)
	for _, k := range peers {
		refs = append(refs, secretRef{"a2a.peers." + k + ".api_key", "A2A peer " + k, cfg.A2A.Peers[k].APIKey})
	}
	h.secretRefs = refs
}

// dbCredentialVerdict classifies the database credential. It is shared by
// db_credential_strength and db_listen_exposure (a known password on an open
// port is the incident, so the exposure check escalates on it).
func (s dbCredentialSnapshot) verdict() (status string, rule string) {
	if s.driver == "sqlite" {
		return "SKIPPED", "driver sqlite has no database credential"
	}
	pw := s.password
	if pw == "" {
		if strings.HasPrefix(s.host, "/") {
			return "SKIPPED", "database.host is a unix socket and database.password is empty: peer authentication, no password to examine"
		}
		return "WARNING", "database.password is empty over TCP: the server must be trusting the connection or reading .pgpass; that was not examined"
	}
	lower := strings.ToLower(pw)
	switch {
	case testLaneDBNames[strings.ToLower(s.user)] && testLaneDBNames[lower]:
		return "ERROR", "database.user and database.password are the integration test lane's default pair; production must never share the test lane's credentials"
	case knownDefaultDBPasswords[lower]:
		return "ERROR", "database.password is a known default that installers, examples or test lanes have shipped"
	case s.user != "" && strings.EqualFold(pw, s.user):
		return "ERROR", "database.password equals database.user"
	case s.name != "" && strings.EqualFold(pw, s.name):
		return "ERROR", "database.password equals database.name"
	case len(pw) < minDBPasswordLen:
		return "WARNING", fmt.Sprintf("database.password is %d characters, under the %d-character floor", len(pw), minDBPasswordLen)
	}
	return "OK", fmt.Sprintf("database.password, %d characters: not a known default, not the role or database name", len(pw))
}

func (h *DoctorHandlers) checkDBCredentialStrength() DoctorCheck {
	const name = "db_credential_strength"
	if !h.dbCreds.known {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "daemon config not captured; database credential not examined"}
	}
	status, rule := h.dbCreds.verdict()
	switch status {
	case "SKIPPED":
		return DoctorCheck{Name: name, Status: status, Message: "0 database credentials examined: " + rule}
	case "OK":
		return DoctorCheck{Name: name, Status: status, Message: "1 database credential examined (" + rule + ")"}
	}
	return DoctorCheck{Name: name, Status: status,
		Message: "1 database credential examined: " + rule + ". Rotate it: docs/operator/2026-10-21-database-credential-retrofit.md"}
}

// credential_reuse --------------------------------------------------------

func (h *DoctorHandlers) checkCredentialReuse() DoctorCheck {
	const name = "credential_reuse"
	if h.secretRefs == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "daemon config not captured; secrets not examined"}
	}
	type group struct {
		purposes map[string]bool
		paths    []string
	}
	byDigest := map[string]*group{}
	var digests []string
	examined, unset := 0, 0
	purposes := map[string]bool{}
	for _, r := range h.secretRefs {
		if r.value == "" {
			unset++
			continue
		}
		examined++
		purposes[r.purpose] = true
		sum := sha256.Sum256([]byte(r.value))
		d := hex.EncodeToString(sum[:])
		g, ok := byDigest[d]
		if !ok {
			g = &group{purposes: map[string]bool{}}
			byDigest[d] = g
			digests = append(digests, d)
		}
		g.purposes[r.purpose] = true
		g.paths = append(g.paths, r.path)
	}
	scope := fmt.Sprintf("%d secret values examined across %d fields (%d unset), %d purposes", examined, len(h.secretRefs), unset, len(purposes))
	notCovered := "; the secret-store key and project secrets are not compared"
	var items []string
	status := "OK"
	for _, d := range digests {
		g := byDigest[d]
		if len(g.purposes) < 2 {
			continue
		}
		internal := g.purposes["database"] || g.purposes["operator API"]
		if internal {
			status = "ERROR"
		} else if status == "OK" {
			status = "WARNING"
		}
		ps := make([]string, 0, len(g.purposes))
		for p := range g.purposes {
			ps = append(ps, p)
		}
		sort.Strings(ps)
		items = append(items, fmt.Sprintf("one value serves %s: %s", strings.Join(ps, " + "), strings.Join(g.paths, ", ")))
	}
	if status == "OK" {
		return DoctorCheck{Name: name, Status: status, Message: scope + "; none shared between purposes" + notCovered}
	}
	return DoctorCheck{Name: name, Status: status, Items: items,
		Message: fmt.Sprintf("%s; %d shared between purposes: give each purpose its own secret%s", scope, len(items), notCovered)}
}

// db_listen_exposure ------------------------------------------------------

// tcpListener is one LISTEN row of /proc/net/tcp{,6}.
type tcpListener struct {
	ip   net.IP
	port int
	v6   bool
}

// parseProcNetTCP reads the LISTEN (state 0A) rows of a /proc/net/tcp or
// /proc/net/tcp6 file. The address is hex, in 32-bit words of host byte order
// (little-endian on every architecture Vornik ships for).
func parseProcNetTCP(data []byte, v6 bool) []tcpListener {
	var out []tcpListener
	for i, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 4 || f[3] != "0A" {
			continue
		}
		hostHex, portHex, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		port, err := strconv.ParseUint(portHex, 16, 16)
		if err != nil {
			continue
		}
		raw, err := hex.DecodeString(hostHex)
		if err != nil || (len(raw) != 4 && len(raw) != 16) {
			continue
		}
		ip := make(net.IP, len(raw))
		for w := 0; w < len(raw); w += 4 {
			ip[w], ip[w+1], ip[w+2], ip[w+3] = raw[w+3], raw[w+2], raw[w+1], raw[w]
		}
		if v4 := ip.To4(); v4 != nil {
			ip = v4 // an IPv4-mapped IPv6 address is read as IPv4
		}
		out = append(out, tcpListener{ip: ip, port: int(port), v6: v6})
	}
	return out
}

func (h *DoctorHandlers) readProcNet(path string) ([]byte, error) {
	if h.procNetReadFunc != nil {
		return h.procNetReadFunc(path)
	}
	return os.ReadFile(path)
}

func (h *DoctorHandlers) localIPs() ([]net.IP, error) {
	if h.interfaceAddrsFunc != nil {
		return h.interfaceAddrsFunc()
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []net.IP
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n.IP)
		}
	}
	return out, nil
}

func (h *DoctorHandlers) lookupHost(ctx context.Context, host string) ([]string, error) {
	if h.lookupHostFunc != nil {
		return h.lookupHostFunc(ctx, host)
	}
	return net.DefaultResolver.LookupHost(ctx, host)
}

// dbHostIsLocal reports whether database.host names this host, and why not
// when it does not.
func (h *DoctorHandlers) dbHostIsLocal(ctx context.Context, host string) (bool, string) {
	if host == "" || strings.HasPrefix(host, "/") || strings.EqualFold(host, "localhost") {
		return true, ""
	}
	h4 := strings.Trim(host, "[]")
	var candidates []net.IP
	if ip := net.ParseIP(h4); ip != nil {
		candidates = []net.IP{ip}
	} else {
		lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		addrs, err := h.lookupHost(lctx, host)
		if err != nil {
			return false, fmt.Sprintf("database host %s did not resolve (%v); its listener was not examined here", host, err)
		}
		for _, a := range addrs {
			if ip := net.ParseIP(a); ip != nil {
				candidates = append(candidates, ip)
			}
		}
	}
	local, _ := h.localIPs()
	for _, c := range candidates {
		// Dialling 0.0.0.0 or :: reaches this host's own listeners (code
		// review round 1, F3: it used to read as "not this host" and skip the
		// very wildcard shape the check exists for).
		if c.IsLoopback() || c.IsUnspecified() {
			return true, ""
		}
		for _, l := range local {
			if l.Equal(c) {
				return true, ""
			}
		}
	}
	return false, fmt.Sprintf("database host %s is not this host; its listener was not examined here", host)
}

func (h *DoctorHandlers) checkDBListenExposure(ctx context.Context) DoctorCheck {
	const name = "db_listen_exposure"
	s := h.dbCreds
	if !s.known {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "daemon config not captured; listener not examined"}
	}
	if s.driver == "sqlite" {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "driver sqlite: no database listener"}
	}
	if ok, why := h.dbHostIsLocal(ctx, s.host); !ok {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: why}
	}
	port := s.port
	if port == 0 {
		port = 5432
	}
	tcp4, err4 := h.readProcNet("/proc/net/tcp")
	tcp6, err6 := h.readProcNet("/proc/net/tcp6")
	if err4 != nil && err6 != nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "listening sockets not examined: " + err4.Error()}
	}
	l4, l6 := parseProcNetTCP(tcp4, false), parseProcNetTCP(tcp6, true)
	all := append(append([]tcpListener{}, l4...), l6...)
	var onPort []tcpListener
	for _, l := range all {
		if l.port == port {
			onPort = append(onPort, l)
		}
	}
	scope := fmt.Sprintf("examined %d listening TCP sockets (tcp4 %d, tcp6 %d); %d on port %d", len(all), len(l4), len(l6), len(onPort), port)
	if err4 != nil || err6 != nil {
		scope += " (one of /proc/net/tcp, /proc/net/tcp6 was unreadable)"
	}
	var exposed []string
	for _, l := range onPort {
		if l.ip.IsLoopback() {
			continue
		}
		addr := l.ip.String()
		if l.ip.IsUnspecified() {
			addr += " (every interface)"
		}
		exposed = append(exposed, addr)
	}
	sort.Strings(exposed)
	switch {
	case len(onPort) == 0 && strings.HasPrefix(s.host, "/"):
		return DoctorCheck{Name: name, Status: "OK", Message: scope + ": Postgres takes no TCP connections in this network namespace"}
	case len(onPort) == 0:
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: scope + ": the daemon reaches the database over TCP but nothing here listens on that port, so the listener is in another network namespace and was not examined"}
	case len(exposed) == 0:
		return DoctorCheck{Name: name, Status: "OK", Message: scope + ", all on loopback"}
	}
	status := "WARNING"
	tail := ""
	if cred, _ := s.verdict(); cred == "ERROR" {
		status = "ERROR"
		tail = " The database credential is also a known or guessable value (db_credential_strength), so anyone who can reach the port can log in."
	}
	return DoctorCheck{Name: name, Status: status, Items: exposed, Message: fmt.Sprintf(
		"%s: Postgres port %d accepts connections on %s. The firewall was not examined. Bind it to 127.0.0.1 (POSTGRES_BIND in the deps .env, or listen_addresses = 'localhost') unless off-host access is intended.%s",
		scope, port, strings.Join(exposed, ", "), tail)}
}
