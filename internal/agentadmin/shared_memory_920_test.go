package agentadmin

import (
	"strings"
	"testing"
)

// GitHub #920 (agent-administered design part 24 §24.6 S3): a project joins
// the namespace's shared memory only with an approval; leaving applies at
// once; a verb that re-renders the project keeps the flag.
func TestSharedMemory_Verbs_920(t *testing.T) {
	tr := newTree(t, "hermes")
	create := tr.render(VerbCreateProject, CreateProjectInput{Slug: "venues", Purpose: "Venue research", SharedMemory: true})
	tr.mustClass(create, Widening)
	if !strings.Contains(create.Sentence, `The project "hermes--venues" (no connections yet) will share memory with: no other project yet.`) ||
		!strings.Contains(create.Sentence, "leaving later does not take back what it stored") {
		t.Fatalf("create sentence: %s", create.Sentence)
	}
	if !strings.Contains(create.Ops[0].Content, "shared_with_namespace: true") {
		t.Fatalf("rendered project:\n%s", create.Ops[0].Content)
	}
	tr.apply(create)

	// set_budget re-renders the project; the flag survives.
	low := tr.render(VerbSetBudget, SetBudgetInput{Project: "venues", MonthlyUSD: 1})
	tr.mustClass(low, Inert)
	if !strings.Contains(low.Ops[0].Content, "shared_with_namespace: true") {
		t.Fatalf("a re-render dropped the flag:\n%s", low.Ops[0].Content)
	}
	tr.apply(low)

	off := false
	leave := tr.render(VerbUpdateProject, UpdateProjectInput{Project: "venues", SharedMemory: &off})
	tr.mustClass(leave, Inert)
	if strings.Contains(leave.Ops[0].Content, "shared_with_namespace") {
		t.Fatalf("leaving kept the flag:\n%s", leave.Ops[0].Content)
	}
	tr.apply(leave)

	on := true
	join := tr.render(VerbUpdateProject, UpdateProjectInput{Project: "venues", SharedMemory: &on})
	tr.mustClass(join, Widening)
	if !strings.Contains(join.Ops[0].Content, "shared_with_namespace: true") || !strings.Contains(join.Sentence, "share memory") {
		t.Fatalf("join: %s\n%s", join.Sentence, join.Ops[0].Content)
	}
}

func TestSharedMemory_Refusals_920(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("venues")
	on := true
	if c := tr.render(VerbUpdateProject, UpdateProjectInput{Project: "home", SharedMemory: &on}); c.Class != Refused {
		t.Fatalf("home: %v %q", c.Class, c.Reason)
	}
	// Plain create stays sealed and inert.
	c := tr.render(VerbCreateProject, CreateProjectInput{Slug: "mail", Purpose: "Read my mail"})
	tr.mustClass(c, Inert)
	if strings.Contains(c.Ops[0].Content, "shared_with_namespace") {
		t.Fatalf("a plain project shares memory:\n%s", c.Ops[0].Content)
	}
}

// review-20261125-99e0 B3: joining names every other shared project and its
// connections, so the person approves the combination they see.
func TestSharedMemory_SentenceNamesTheOtherSharedProjects_920(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.apply(tr.render(VerbCreateProject, CreateProjectInput{Slug: "scout", Purpose: "Find venues", SharedMemory: true}))
	tr.approveServer("scout", "scraper", "https://scraper.example/mcp", "web_fetch")
	tr.project("mail")
	on := true
	join := tr.render(VerbUpdateProject, UpdateProjectInput{Project: "mail", SharedMemory: &on})
	tr.mustClass(join, Widening)
	if !strings.Contains(join.Sentence, `will share memory with: "hermes--scout" (connections: scraper)`) {
		t.Fatalf("sentence: %s", join.Sentence)
	}
}
