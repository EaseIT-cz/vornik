package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
)

// GitHub #412 (found at: production, operator observation 2026-10-16). The
// reference host ran its production database on the integration lane's
// credential pair, with Postgres listening on 0.0.0.0:5432, and `vornikctl
// doctor` reported nothing. These tests drive POST /api/v1/doctor, the surface
// the operator reads, with the reference host's shape and its neighbours.
// Design: https://docs.vornik.io §2.1 D5.

// procTCP renders /proc/net/tcp rows: each entry is "HEXADDR:HEXPORT ST".
func procTCP(rows ...string) []byte {
	var b strings.Builder
	b.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	for i, r := range rows {
		f := strings.Fields(r)
		b.WriteString("   " + string(rune('0'+i)) + ": " + f[0] + " 00000000:0000 " + f[1] + " 00000000:00000000 00:00000000 00000000  1000        0 1 1 0000000000000000 100 0 0 10 0\n")
	}
	return []byte(b.String())
}

func procTCP6(rows ...string) []byte {
	var b strings.Builder
	b.WriteString("  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	for i, r := range rows {
		f := strings.Fields(r)
		b.WriteString("   " + string(rune('0'+i)) + ": " + f[0] + " 00000000000000000000000000000000:0000 " + f[1] + " 00000000:00000000 00:00000000 00000000  1000        0 1 1 0000000000000000 100 0 0 10 0\n")
	}
	return []byte(b.String())
}

const (
	// 5432 = 0x1538.
	tcpWildcard5432  = "00000000:1538 0A"
	tcpLoopback5432  = "0100007F:1538 0A"
	tcpLAN5432       = "0A00A8C0:1538 0A" // 192.168.0.10
	tcpWildcardOther = "00000000:1F90 0A" // 8080
	tcpEstablished   = "00000000:1538 01" // not LISTEN
	tcp6Wildcard5432 = "00000000000000000000000000000000:1538 0A"
	tcp6Loopback5432 = "00000000000000000000000001000000:1538 0A"
	tcp6Mapped127    = "0000000000000000FFFF00000100007F:1538 0A"
)

type dbDoctorFixture struct {
	cfg   *config.Config
	tcp   []byte
	tcp6  []byte
	ifcs  []string
	hosts map[string][]string
	read  error
}

func baseDBConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Database = config.DatabaseConfig{Driver: "postgres", Host: "localhost", Port: 5432, Name: "vornik", User: "vornik",
		Password: "c2f0e9a1b7d4436a9e5b8f20d1c7a6e3"}
	cfg.API.AuthEnabled = true
	cfg.API.APIKeys = []string{"sk-vornik-operator-0123456789abcdef0123"}
	return cfg
}

func runDBDoctor(t *testing.T, fx dbDoctorFixture) (map[string]DoctorCheck, string) {
	t.Helper()
	h := NewDoctorHandlers(closedDB(t))
	h.SetServerConfig(fx.cfg)
	h.procNetReadFunc = func(path string) ([]byte, error) {
		if fx.read != nil {
			return nil, fx.read
		}
		if strings.HasSuffix(path, "tcp6") {
			return fx.tcp6, nil
		}
		return fx.tcp, nil
	}
	h.interfaceAddrsFunc = func() ([]net.IP, error) {
		var out []net.IP
		for _, s := range fx.ifcs {
			out = append(out, net.ParseIP(s))
		}
		return out, nil
	}
	h.lookupHostFunc = func(_ context.Context, host string) ([]string, error) {
		if a, ok := fx.hosts[host]; ok {
			return a, nil
		}
		return nil, errors.New("no such host")
	}
	rec := httptest.NewRecorder()
	h.RunDoctor(rec, authDisabledReq(httptest.NewRequest(http.MethodPost, "/api/v1/doctor", nil)))
	require.Equal(t, http.StatusOK, rec.Code)
	var report DoctorReport
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &report))
	return checkNames(report), rec.Body.String()
}

// The reference host, as observed: role swarmd, password swarmd, Postgres on
// 0.0.0.0:5432. Every new check must say so, and the listener is an ERROR
// because a known password sits on an open port.
func TestDoctor412_ReferenceHostShapeIsReported(t *testing.T) {
	cfg := baseDBConfig()
	cfg.Database.User, cfg.Database.Password, cfg.Database.Name = "swarmd", "swarmd", "vornik_it"
	checks, _ := runDBDoctor(t, dbDoctorFixture{cfg: cfg, tcp: procTCP(tcpWildcard5432), tcp6: procTCP6()})

	strength := checks["db_credential_strength"]
	assert.Equal(t, "ERROR", strength.Status, strength.Message)
	assert.Contains(t, strength.Message, "1 database credential examined")
	assert.Contains(t, strings.ToLower(strength.Message+strings.Join(strength.Items, " ")), "test lane")

	exposure := checks["db_listen_exposure"]
	assert.Equal(t, "ERROR", exposure.Status, exposure.Message)
	assert.Contains(t, exposure.Message, "0.0.0.0")
	assert.Contains(t, exposure.Message, "examined 1 listening TCP socket")
	assert.Contains(t, exposure.Message, "firewall was not examined")

	// The password IS the role name and a known default here, so naming the
	// role, the match or the database would print the live credential (design
	// review round 1, finding b).
	for _, name := range []string{"db_credential_strength", "db_listen_exposure", "credential_reuse"} {
		c := checks[name]
		out := c.Message + " " + strings.Join(c.Items, " ")
		assert.NotContains(t, out, "swarmd", "%s must not print the credential, the role or the database name", name)
	}
}

// The quickstart's rootless podman (netavark + pasta) publishes 0.0.0.0:5432 as
// a dual-stack "::" LISTEN row in /proc/net/tcp6, not as a tcp4 row (measured on
// the host class, 2026-10-21). It must be read as every interface.
func TestDoctor412_PastaDualStackPublishIsExposure(t *testing.T) {
	cfg := baseDBConfig()
	cfg.Database.Password = "vornik_dev_password"
	checks, _ := runDBDoctor(t, dbDoctorFixture{cfg: cfg, tcp: procTCP(), tcp6: procTCP6(tcp6Wildcard5432)})
	c := checks["db_listen_exposure"]
	assert.Equal(t, "ERROR", c.Status, c.Message)
	assert.Contains(t, c.Message, "tcp6 1")
}

func TestDoctor412_CredentialStrengthTable(t *testing.T) {
	cases := []struct {
		name, user, pass, db, host, driver, want string
	}{
		{"strong", "vornik", "c2f0e9a1b7d4436a9e5b8f20d1c7a6e3", "vornik", "localhost", "postgres", "OK"},
		{"quickstart legacy default", "vornik", "vornik_dev_password", "vornik", "localhost", "postgres", "ERROR"},
		{"compiled default", "vornik", "vornik", "vornik", "localhost", "postgres", "ERROR"},
		{"known default any case", "quuxrole", "PostGres", "quuxdb", "localhost", "postgres", "ERROR"},
		{"test lane pair vornik", "vornik", "vornik", "quuxdb", "localhost", "postgres", "ERROR"},
		{"test lane pair swarmd", "swarmd", "swarmd", "vornik_it", "localhost", "postgres", "ERROR"},
		{"equals role", "appuserqx", "appuserqx", "quuxdb", "localhost", "postgres", "ERROR"},
		{"equals database name", "quuxrole", "productiondb", "productiondb", "localhost", "postgres", "ERROR"},
		{"short", "quuxrole", "Zq8!kL2m", "quuxdb", "localhost", "postgres", "WARNING"},
		{"empty over tcp", "quuxrole", "", "quuxdb", "127.0.0.1", "postgres", "WARNING"},
		{"empty over unix socket", "quuxrole", "", "quuxdb", "/run/postgresql", "postgres", "SKIPPED"},
		{"sqlite", "", "", "", "", "sqlite", "SKIPPED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseDBConfig()
			cfg.Database = config.DatabaseConfig{Driver: tc.driver, Host: tc.host, Port: 5432, Name: tc.db, User: tc.user, Password: tc.pass}
			checks, body := runDBDoctor(t, dbDoctorFixture{cfg: cfg, tcp: procTCP(tcpLoopback5432), tcp6: procTCP6()})
			c := checks["db_credential_strength"]
			assert.Equal(t, tc.want, c.Status, c.Message)
			// No value, role or database name in the output of any of the three
			// checks: when the password equals one of them, naming it prints the
			// credential (design review round 1 finding b, round 2 F1/F2).
			for _, name := range []string{"db_credential_strength", "db_listen_exposure", "credential_reuse"} {
				out := checks[name].Message + " " + strings.Join(checks[name].Items, " ")
				for _, v := range []string{tc.pass, tc.user, tc.db} {
					if v != "" {
						assert.NotContains(t, out, v, "%s printed a credential, role or database name", name)
					}
				}
			}
			if len(tc.pass) >= 8 {
				assert.NotContains(t, body, tc.pass, "the doctor must never print the password")
			}
		})
	}
}

// API auth off must not short-circuit the database checks (design review
// round 2, F4: api_key_strength returns OK when auth is off).
func TestDoctor412_DBChecksIgnoreAPIAuthToggle(t *testing.T) {
	cfg := baseDBConfig()
	cfg.API.AuthEnabled = false
	cfg.Database.Password = "vornik"
	checks, _ := runDBDoctor(t, dbDoctorFixture{cfg: cfg, tcp: procTCP(tcpWildcard5432), tcp6: procTCP6()})
	assert.Equal(t, "ERROR", checks["db_credential_strength"].Status)
	assert.Equal(t, "ERROR", checks["db_listen_exposure"].Status)
}

func TestDoctor412_ListenExposureTable(t *testing.T) {
	cases := []struct {
		name      string
		host      string
		port      int
		tcp, tcp6 []byte
		ifcs      []string
		hosts     map[string][]string
		read      error
		want      string
		contains  string
	}{
		{name: "loopback only", host: "127.0.0.1", tcp: procTCP(tcpLoopback5432), tcp6: procTCP6(tcp6Loopback5432), want: "OK", contains: "examined 2 listening TCP sockets"},
		{name: "v4-mapped loopback is loopback", host: "localhost", tcp: procTCP(), tcp6: procTCP6(tcp6Mapped127), want: "OK"},
		{name: "ipv6 wildcard", host: "localhost", tcp: procTCP(tcpLoopback5432), tcp6: procTCP6(tcp6Wildcard5432), want: "WARNING", contains: "::"},
		{name: "specific LAN address", host: "localhost", tcp: procTCP(tcpLAN5432), tcp6: procTCP6(), want: "WARNING", contains: "192.168.0.10"},
		{name: "other ports and non-LISTEN rows ignored", host: "localhost", tcp: procTCP(tcpWildcardOther, tcpEstablished, tcpLoopback5432), tcp6: procTCP6(), want: "OK", contains: "1 on port 5432"},
		{name: "non-default port", host: "localhost", port: 8080, tcp: procTCP(tcpWildcardOther, tcpLoopback5432), tcp6: procTCP6(), want: "WARNING", contains: "port 8080"},
		{name: "db host is this host's LAN address", host: "192.168.0.10", ifcs: []string{"192.168.0.10"}, tcp: procTCP(tcpWildcard5432), tcp6: procTCP6(), want: "WARNING"},
		{name: "db host name resolving to this host", host: "db.lan", ifcs: []string{"192.168.0.10"}, hosts: map[string][]string{"db.lan": {"192.168.0.10"}}, tcp: procTCP(tcpWildcard5432), tcp6: procTCP6(), want: "WARNING"},
		// Dialling 0.0.0.0 or :: reaches this host (code review round 1, F3).
		{name: "db host is the ipv4 wildcard", host: "0.0.0.0", tcp: procTCP(tcpWildcard5432), tcp6: procTCP6(), want: "WARNING", contains: "every interface"},
		{name: "db host is the ipv6 wildcard", host: "[::]", tcp: procTCP(), tcp6: procTCP6(tcp6Wildcard5432), want: "WARNING", contains: "every interface"},
		{name: "remote db host", host: "db.example.net", hosts: map[string][]string{"db.example.net": {"203.0.113.7"}}, tcp: procTCP(tcpWildcard5432), tcp6: procTCP6(), want: "SKIPPED", contains: "not this host"},
		{name: "unresolvable db host", host: "nowhere.invalid", tcp: procTCP(tcpWildcard5432), tcp6: procTCP6(), want: "SKIPPED"},
		{name: "unix socket, no tcp listener", host: "/run/postgresql", tcp: procTCP(tcpWildcardOther), tcp6: procTCP6(), want: "OK", contains: "no TCP"},
		{name: "unix socket but tcp wildcard listener", host: "/run/postgresql", tcp: procTCP(tcpWildcard5432), tcp6: procTCP6(), want: "WARNING"},
		{name: "tcp host, no listener in this namespace", host: "127.0.0.1", tcp: procTCP(tcpWildcardOther), tcp6: procTCP6(), want: "SKIPPED", contains: "another network namespace"},
		{name: "proc unreadable", host: "localhost", read: errors.New("open /proc/net/tcp: no such file or directory"), want: "SKIPPED", contains: "not examined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseDBConfig()
			cfg.Database.Host = tc.host
			if tc.port != 0 {
				cfg.Database.Port = tc.port
			}
			checks, _ := runDBDoctor(t, dbDoctorFixture{cfg: cfg, tcp: tc.tcp, tcp6: tc.tcp6, ifcs: tc.ifcs, hosts: tc.hosts, read: tc.read})
			c := checks["db_listen_exposure"]
			assert.Equal(t, tc.want, c.Status, c.Message)
			if tc.contains != "" {
				assert.Contains(t, c.Message+" "+strings.Join(c.Items, " "), tc.contains)
			}
		})
	}
}

func TestDoctor412_ListenExposureSQLiteSkipped(t *testing.T) {
	cfg := baseDBConfig()
	cfg.Database = config.DatabaseConfig{Driver: "sqlite", Path: "/tmp/x.db"}
	checks, _ := runDBDoctor(t, dbDoctorFixture{cfg: cfg, tcp: procTCP(tcpWildcard5432), tcp6: procTCP6()})
	assert.Equal(t, "SKIPPED", checks["db_listen_exposure"].Status)
}

func TestDoctor412_CredentialReuse(t *testing.T) {
	const shared = "Shared-Secret-Value-9f8e7d6c5b4a39281706"
	t.Run("database password reused as API key is ERROR and never printed", func(t *testing.T) {
		cfg := baseDBConfig()
		cfg.Database.Password = shared
		cfg.API.APIKeys = []string{shared}
		checks, body := runDBDoctor(t, dbDoctorFixture{cfg: cfg, tcp: procTCP(tcpLoopback5432), tcp6: procTCP6()})
		c := checks["credential_reuse"]
		assert.Equal(t, "ERROR", c.Status, c.Message)
		joined := strings.Join(c.Items, " ")
		assert.Contains(t, joined, "database.password")
		assert.Contains(t, joined, "api.api_keys")
		assert.NotContains(t, body, shared)
	})
	t.Run("api key doubling as admin key is one purpose", func(t *testing.T) {
		cfg := baseDBConfig()
		cfg.Admin.AllowedKeys = []string{cfg.API.APIKeys[0]}
		checks, _ := runDBDoctor(t, dbDoctorFixture{cfg: cfg, tcp: procTCP(tcpLoopback5432), tcp6: procTCP6()})
		c := checks["credential_reuse"]
		assert.Equal(t, "OK", c.Status, c.Message)
		assert.Contains(t, c.Message, "examined")
	})
	t.Run("one LLM key across call sites is one purpose", func(t *testing.T) {
		cfg := baseDBConfig()
		cfg.Chat.APIKey = "sk-provider-key-aaaaaaaaaaaaaaaaaaaaaaaa"
		cfg.Runtime.AgentLLM.APIKey = cfg.Chat.APIKey
		cfg.Memory.EmbeddingAPIKey = cfg.Chat.APIKey
		checks, _ := runDBDoctor(t, dbDoctorFixture{cfg: cfg, tcp: procTCP(tcpLoopback5432), tcp6: procTCP6()})
		assert.Equal(t, "OK", checks["credential_reuse"].Status, checks["credential_reuse"].Message)
	})
	t.Run("LLM key reused as telegram token is a third-party WARNING", func(t *testing.T) {
		cfg := baseDBConfig()
		cfg.Chat.APIKey = shared
		cfg.Telegram.BotToken = shared
		checks, body := runDBDoctor(t, dbDoctorFixture{cfg: cfg, tcp: procTCP(tcpLoopback5432), tcp6: procTCP6()})
		assert.Equal(t, "WARNING", checks["credential_reuse"].Status, checks["credential_reuse"].Message)
		assert.NotContains(t, body, shared)
	})
	t.Run("denominator counts set and unset fields", func(t *testing.T) {
		cfg := baseDBConfig()
		checks, _ := runDBDoctor(t, dbDoctorFixture{cfg: cfg, tcp: procTCP(tcpLoopback5432), tcp6: procTCP6()})
		c := checks["credential_reuse"]
		assert.Equal(t, "OK", c.Status)
		// baseDBConfig sets exactly two secrets: database.password and api.api_keys[0].
		assert.Contains(t, c.Message, "2 secret values examined")
		assert.Contains(t, c.Message, "unset")
	})
}
