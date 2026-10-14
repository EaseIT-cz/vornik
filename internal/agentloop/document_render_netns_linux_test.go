//go:build linux

package agentloop

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// audit 2026-10-12 (#339): document_render's pandoc and python3 children ran
// with the agent container's network. No request was made only because the
// filter and the deny-all fetcher held; the children themselves could reach
// any address the container could. Each child now starts in a fresh user and
// network namespace. The stubbed child below connects straight to a listener
// this test opened on loopback: before the fix the connection arrives.
func TestDocumentRenderChildrenHaveNoNetwork(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed for the connecting child")
	}
	probe := "import socket,sys\ntry:\n socket.create_connection(('127.0.0.1'," + port + "),2)\n print('connected',file=sys.stderr)\nexcept OSError:\n pass\nsys.stdout.write(sys.stdin.read())\n"
	r := documentRenderer{timeout: 10 * time.Second, command: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, python, "-c", probe)
	}}
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "report.md"), "# Praha\n")
	raw, _ := json.Marshal(documentRenderArgs{Path: "report.md", Format: "html"})
	got := renderDocumentWith(Env{Workspace: ws}, raw, r)
	var out documentRenderResult
	if err := json.Unmarshal([]byte(got), &out); err != nil || !out.OK {
		t.Fatalf("isolated render failed (unprivileged user namespaces are required): %s", got)
	}
	if err := ln.(*net.TCPListener).SetDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if conn, err := ln.Accept(); err == nil {
		_ = conn.Close()
		t.Fatal("a renderer child reached the network: it connected to the test's loopback listener")
	}
}

// review-20261012-b33c F2: where the kernel or seccomp refuses an unprivileged
// user namespace, the child must not start and nothing un-isolated runs. The
// refusal is simulated by a uid mapping the kernel will not grant an
// unprivileged process (a host uid that is not ours), which fails at Start
// exactly as a refused CLONE_NEWUSER does.
func TestDocumentRenderFailsClosedWithoutNamespaces(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root may map any uid")
	}
	orig := documentIsolate
	t.Cleanup(func() { documentIsolate = orig })
	documentIsolate = func(cmd *exec.Cmd) error {
		if err := isolateDocumentChild(cmd); err != nil {
			return err
		}
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid() + 1, Size: 1}}
		return nil
	}
	ran := filepath.Join(t.TempDir(), "child-ran")
	r := documentRenderer{timeout: 10 * time.Second, command: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", "touch '"+ran+"'; cat")
	}}
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "report.md"), "# Praha\n")
	raw, _ := json.Marshal(documentRenderArgs{Path: "report.md", Format: "html"})
	var out documentRenderResult
	if err := json.Unmarshal([]byte(renderDocumentWith(Env{Workspace: ws}, raw, r)), &out); err != nil {
		t.Fatal(err)
	}
	if out.OK || out.Stage != "pandoc" || out.Error == "" {
		t.Fatalf("a refused namespace must fail the render: %+v", out)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the renderer child ran without its namespace")
	}
	if _, err := os.Stat(filepath.Join(ws, "artifacts/out/report.html")); !os.IsNotExist(err) {
		t.Fatalf("output written despite the refusal: %v", err)
	}
}
