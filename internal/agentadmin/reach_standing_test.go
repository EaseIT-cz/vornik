package agentadmin

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// Broker write-actions design, "Tier 2, revised" item 1 and review 5c20 F6:
// an absent standing leaves an approved workflow's reach hash unchanged, and
// declaring standing changes it (it is widening, approved like any reach
// change). reachGoldenNoStanding was computed by this test's fixture on
// main at 68c7c2169, before standing existed: an approval stored then still
// verifies. (The hash is computed from the loaded config, never stored by a
// table migration 213 touches, so this golden is the cross-version check.)
const reachGoldenNoStanding = "3ecd50b6fed7db1044b9f0bc630950cdc61a5953fa42fdb1f22e9ba852d6222c"

const reachFixture = `---
workflowId: "ns1--mail"
version: 1.0.0
entrypoint: draft
broker:
  input_schema: {"type":"object","additionalProperties":false,"properties":{}}
  egress:
    output: "result.json"
    max_bytes: 4096
    schema: {"type":"object"}
  proposes:
    - action: "send_reply"
      tool: "mcp__mail-write__gmail_send"
      output: "propose-send_reply.json"
      args_schema: {"type":"object","additionalProperties":false,"properties":{"to":{"type":"string","format":"email","maxLength":254,"x-destination":true},"body":{"type":"string","maxLength":4000,"x-untrusted":true}}}
STANDING
steps:
  draft:
    type: agent
    role: "drafter"
    on_success: done
---

## Prompts

### draft

Draft it.
`

func reachHashOf(t *testing.T, standing string) string {
	t.Helper()
	md := strings.Replace(reachFixture, "STANDING\n", standing, 1)
	wf, err := registry.ParseWorkflowMarkdown([]byte(md), "ns1--mail.md")
	if err != nil {
		t.Fatal(err)
	}
	sig, err := SignatureOf(nil, nil, wf)
	if err != nil {
		t.Fatal(err)
	}
	return sig.Hash()
}

func TestReach_AbsentStandingKeepsTheHash(t *testing.T) {
	if got := reachHashOf(t, ""); got != reachGoldenNoStanding {
		t.Fatalf("reach hash of a workflow without standing = %s, want the pre-tier-2 %s", got, reachGoldenNoStanding)
	}
}

func TestReach_DeclaringStandingIsWidening(t *testing.T) {
	without := reachHashOf(t, "")
	with := reachHashOf(t, "      standing: {key: [to]}\n")
	other := reachHashOf(t, "      standing: {key: [to], max_uses: 5}\n")
	if with == without || other == with {
		t.Fatal("declaring or changing standing did not change the reach hash")
	}
}
