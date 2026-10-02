# Agent templates

The only templates the agent admin verbs render from (agent-administered
Vornik design §7.4). They contain no ID literals: every ID and reference is a
parameter the renderer fills from the agent's namespace, and a test renders
each one with a dummy namespace and checks every ID carries its prefix.
Edit with care: these files decide what an agent-created project looks like.
