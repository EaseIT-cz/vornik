// vornik-companion: the Vornik companion plugin for opencode (LLD 29, GitHub #841).
// Installed by `vornikctl companion connect opencode`, which writes the three
// constants below for this machine; edit nothing here by hand.
//
// It appends Vornik's working rules and a session digest to opencode's system
// prompt. Appended to the LAST existing system string: on opencode 1.14.37 a
// pushed entry did not reach the model, appended text did (LLD 29 A2). The
// digest comes from `vornikctl companion session-start`, fetched once per
// session; the key stays in the file connect wrote and never passes through
// here. Nothing in this plugin throws into opencode.
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const VERSION = "0.1.0";
const TESTED_OPENCODE = "1.14.37";
void VERSION; void TESTED_OPENCODE; // read by vornikctl companion doctor, not here

const VORNIKCTL = "vornikctl";
const KEY_NAME = "default";
const VORNIK_URL = "";

const DIGEST_TIMEOUT_MS = 8000;
const FALLBACK =
  "Vornik: the session digest is unavailable; call Vornik's recent_memory tool to catch up.";

function readGuidance() {
  try {
    const here = dirname(fileURLToPath(import.meta.url));
    return readFileSync(join(here, "vornik-companion-guidance.md"), "utf8");
  } catch {
    return "";
  }
}

const guidance = readGuidance();
const digests = new Map();

function withTimeout(promise, ms) {
  return Promise.race([
    promise,
    new Promise((resolve) => setTimeout(() => resolve(null), ms)),
  ]);
}

function digestFor($, sessionID) {
  if (digests.has(sessionID)) return digests.get(sessionID);
  const pending = (async () => {
    try {
      const args = ["companion", "session-start", "--name", KEY_NAME];
      if (VORNIK_URL) args.push("--url", VORNIK_URL);
      const out = await withTimeout($`${VORNIKCTL} ${args}`.quiet().nothrow().text(), DIGEST_TIMEOUT_MS);
      if (!out) return FALLBACK;
      return JSON.parse(out).additional_context || FALLBACK;
    } catch {
      return FALLBACK;
    }
  })();
  digests.set(sessionID, pending);
  return pending;
}

export const VornikCompanion = async ({ $ }) => ({
  "experimental.chat.system.transform": async (input, output) => {
    try {
      const digest = await digestFor($, (input && input.sessionID) || "no-session");
      const text = "\n\n" + guidance + "\n\n" + digest;
      if (output.system.length === 0) output.system.push(text);
      else output.system[output.system.length - 1] += text;
    } catch {
      // Never break opencode's request.
    }
  },
});
