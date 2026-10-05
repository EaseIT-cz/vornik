#!/usr/bin/env bash
# Regression: September 2026 audit found unused binaries, partial dispatch
# verification, duplicate unsigned publishing and PR cache uploads.
set -euo pipefail
[ -f .goreleaser.enterprise.yaml ] || exit 0
python3 - <<'PY'
from pathlib import Path
import yaml
root=Path(__file__).resolve().parent if False else Path.cwd()
def read(p): return yaml.load((root/p).read_text(), Loader=yaml.BaseLoader)
ci=read('.github/workflows/ci.yaml')
for job in ci['jobs'].values():
 for step in job.get('steps',[]):
  if 'actions/upload-artifact@' in step.get('uses',''):
   assert step['with']['name'] != 'vornik-binary', 'unused binary artifact'
   assert step['with'].get('retention-days') == '1'
   assert 'full' in step.get('if',''), 'partial PR artifact has no consumer'
assert 'workflow_dispatch' in str(ci['jobs']['changes']['steps']), 'dispatch must run full tests'

# --- no sync/release-* branch gate any more -----------------------------------
# The EE mirror's sync PRs are retired with the mirror (EaseIT-cz migration
# design §6). No push trigger for sync branches, and no PR-side skip on
# `changes` that a fork could claim by naming its branch.
push_branches = ci['on' if 'on' in ci else True]['push']['branches']
assert not any('sync/' in b for b in push_branches), \
    'the retired sync/release-* branches must not trigger CI'
assert 'if' not in ci['jobs']['changes'] or 'sync/release-' not in ci['jobs']['changes']['if'], \
    'the changes job must not carry the retired sync-PR skip'
# verify is the single required check. If it could pass while `changes` was
# skipped, a skipped sync PR would report a green aggregate over zero jobs.
verify_if = ci['jobs']['verify']['if']
assert "needs.changes.result != 'skipped'" in verify_if, \
    'verify must skip in lockstep with changes, never report success over a skipped run'

# EVERY job must reach the aggregate, and every needed job must be reported.
# Named generically on purpose: an allowlist of job names is the same drift
# hazard the job list itself is. `ce-export-verify` — the CE operator-token leak
# scan and the Art 50 parity self-test — was the one job outside verify's needs
# from its introduction until 2026-09-06, so a leak-scan failure left the single
# aggregate check green.
jobs = set(ci['jobs'])
needs = set(ci['jobs']['verify']['needs'])
results = ci['jobs']['verify']['steps'][0]['env']['RESULTS']
reported = {line.split('=')[0].strip() for line in results.split() if '=' in line}
assert not (jobs - needs - {'verify'}), \
    f'job(s) outside the verify aggregate: {sorted(jobs - needs - {"verify"})}'
assert not (needs - reported), \
    f'job(s) verify waits on but never reports: {sorted(needs - reported)}'
r=read('.github/workflows/release.yaml')
assert not Path('.github/workflows/release-enterprise.yaml').exists(), 'duplicate package publisher'
assert 'EaseIT-cz/vornik' in r['jobs']['goreleaser']['if']
assert 'GPG_PRIVATE_KEY' in str(r)
assert '--skip=sign' not in str(r)
assert 'Verify full CI' in str(r)
a=read('.github/actions/setup-go/action.yaml')
assert a['inputs']['cache-write']['default'] == 'false'
assert 'refs/heads/main' in str(a)
assert 'actions/cache/restore@' in str(a)
# The CE tag step. Nothing created the CE tag until 2026-09-06: the export
# pushed to main and stopped, every previous tag was made by hand, and 2026.9.3
# shipped with no public tag and no version-tagged agent image because of it.
# A documented step with no mechanism is a step that is sometimes skipped.
ce=read('.github/workflows/publish-ce.yaml')
steps=ce['jobs']['publish-ce']['steps'] if 'publish-ce' in ce['jobs'] else list(ce['jobs'].values())[0]['steps']
tag_steps=[s for s in steps if 'tag' in s.get('name','').lower()]
assert tag_steps, 'publish-ce must tag the exported CE tree'
body=str(tag_steps[0])
assert 'release.tag_name' in body, 'the tag must come from the release that triggered the publish'
# The step delegates to the export script's --push-tag (EaseIT-cz migration
# plan T6.3), so idempotency and never-move are asserted where they live.
assert '--push-tag' in body, 'the CE tag step must push through export-public-ce.sh --push-tag'
export_src=(root/'scripts/export-public-ce.sh').read_text()
push_tag_fn=export_src[export_src.index('push_ce_tag() {'):export_src.index('\n}\n', export_src.index('push_ce_tag() {'))]
assert 'ls-remote' in push_tag_fn, 'the CE tag push must be idempotent — a re-run must not fail on an existing tag'
# A release tag names one artifact forever. Moving it makes a published name
# mean something new, which is the one thing a tag must never do.
assert '--force' not in push_tag_fn and ' -f ' not in push_tag_fn, 'the CE tag must never be moved'
# The publish token travels only as a per-command header (plan T6.3, review
# 1c96 M4): every network git operation in the export goes through ce_git,
# and no remote URL carries a credential.
import re as _re
for line in export_src.splitlines():
    code=line.split('#',1)[0]
    if _re.search(r'(?<![\w-])git((\s+-[Cc]\s+\S+)|(\s+-q))*\s+(push|clone|ls-remote)\b', code) and 'ce_git' not in code:
        raise AssertionError(f'a network git operation bypasses ce_git: {line.strip()}')
assert 'x-access-token:${' not in export_src.replace('printf \'x-access-token:%s\'',''), 'no URL may embed the token'
# Not in argv either: `git -c` puts the header in the process's command line
# (review e9a0 follow-up); ce_git passes it through GIT_CONFIG_* instead.
assert 'git -c "http' not in export_src and 'GIT_CONFIG_VALUE_0=' in export_src, \
    'the publish token must reach git through GIT_CONFIG_*, never `git -c` (argv is world-readable)'
# publish-ce authenticates with the PAT; the org disables deploy keys (C1).
ce_text=(root/'.github/workflows/publish-ce.yaml').read_text()
assert 'secrets.CE_PUBLISH_TOKEN' in ce_text and 'CE_DEPLOY_KEY' not in ce_text and 'setup-publish-ssh' not in ce_text, \
    'publish-ce must use CE_PUBLISH_TOKEN, not a deploy key'
assert 'CE_REPO: EaseIT-cz/vornik' in ce_text, 'publish-ce must target EaseIT-cz/vornik'
# The public repository's own workflows guard on its new name.
for tmpl in ('publish-release.yml','publish-agent-image.yml','cla.yml'):
    t=(root/'scripts/public-ce-templates'/tmpl).read_text()
    assert "grinco/vornik" not in t, f'{tmpl} still names the old repository'
pr_guard=read('scripts/public-ce-templates/publish-release.yml')['jobs']
assert all("EaseIT-cz/vornik" in str(j.get('if','')) for j in pr_guard.values() if 'github.repository' in str(j.get('if',''))), \
    'publish-release guards must name EaseIT-cz/vornik'
# The release job DISPATCHES publish-ce (a GITHUB_TOKEN-published release fires
# no release event), so on every automated release `github.event.release` is
# empty and the tag step reads `inputs.tag`. 2026.9.4 shipped with no CE tag —
# the 2026.9.3 gap again, through the other door — until a second, manual
# dispatch carried `-f tag=`. The release job must pass it.
dispatch=[s for s in r['jobs']['goreleaser']['steps'] if 'fan-out' in s.get('name','').lower()]
assert dispatch, 'release.yaml must dispatch the publication fan-out'
assert 'tag=' in str(dispatch[0]), 'release.yaml must pass the release tag to publish-ce (-f tag=…), or the CE tree is never tagged'

assert 'publish-ce.yaml' in str(dispatch[0]), \
    'release.yaml must dispatch publish-ce.yaml, or no release is ever exported to CE'

# --- 2026-09-22: assert the PUBLISHER SET, not a list of names ---------------
#
# The two assertions above name two arms. The fan-out dispatches THREE — docs
# is the one nobody pinned, and the 2026.9.5 notes recorded the work as "BOTH
# publication arms are now asserted". Two of three was written down as all of
# them, and the arm left out publishes the customer-facing documentation site:
# delete its line and every test here still passes while docs.vornik.io
# silently stops tracking releases.
#
# A three-name list drifts the day a fourth publisher is added, exactly as the
# two-name list did. §C of enterprise-packaging-design.md already settled this
# for a different check: assert the property GENERICALLY, "because an allowlist
# of job names carries exactly the drift hazard the omission came from". So
# this is a SET EQUALITY against the designated publishers, and adding a
# publisher without dispatching it fails here rather than in production.
PUBLISHERS={'docs.yaml','publish-ce.yaml'}
dispatched={w for w in PUBLISHERS|{'ci.yaml','release.yaml','release-upstream-pr.yaml','upstream-sync-pr.yaml'}
            if w in str(dispatch[0])}
assert dispatched == PUBLISHERS, (
    'the release fan-out must dispatch exactly the designated publishers; '
    f'dispatched={sorted(dispatched)} designated={sorted(PUBLISHERS)}')

# --- The retired EE mirror stays retired (EaseIT-cz migration design §6) -----
#
# One Enterprise repository. The mirror's workflows, its deploy key and the
# sync-PR job are gone; none may come back by an edit that forgets why.
import glob as _glob
for wf in _glob.glob(str(root/'.github/workflows/*.y*ml')):
    body=open(wf).read()
    for retired in ('grinco/vornik-ee','UPSTREAM_DEPLOY_KEY','CE_DEPLOY_KEY','release-upstream-pr','upstream-sync-pr','sync-pr-note','sync/release-'):
        assert retired not in body, f'{wf} references the retired mirror machinery: {retired}'
for gone in ('.github/workflows/release-upstream-pr.yaml','.github/workflows/upstream-sync-pr.yaml'):
    assert not (root/gone).exists(), f'{gone} must stay deleted'

# --- Gap 3: the installers must be stamped for the tag being released --------
#
# RELEASE.md step 1 says to run `make quickstart-stamp-ref REF=<tag>`. It was
# not run for 2026.9.5, so for five days from 2026-09-17 every fresh Podman and
# macOS install defaulted to the 2026.9.4 tree — two releases behind what the
# notes described. Nothing failed loudly, because a stale pin installs a
# working system, just the wrong one.
#
# The release job is the one place that holds both the tag and the stamped ref.
rbody=str(r['jobs']['goreleaser']['steps'])
assert 'DEFAULT_VORNIK_REF' in rbody, \
    'release.yaml must verify the podman installer is stamped for this tag'
assert 'deployments/macos/install.sh' in rbody, \
    'release.yaml must verify the macOS installer too — stamping only podman left it behind before'

# AND IT MUST RUN BEFORE ANYTHING PUBLISHES. The first version of this gate sat
# after "Publish signed checksums and release", so it reported on a release the
# world could already download — the exact defect that moved the mirror-merge
# check out of the fan-out and into a separate audit, reintroduced two gaps
# later in the same change. A gate downstream of the thing it gates is a
# notification.
names=[st.get('name','') for st in r['jobs']['goreleaser']['steps']]
stamp_at=next(i for i,n in enumerate(names) if 'stamped for this tag' in n)
publish_at=next(i for i,n in enumerate(names) if n.startswith('Publish'))
build_at=next(i for i,n in enumerate(names) if n.startswith('Build'))
assert stamp_at < build_at < publish_at, (
    'the installer-stamp gate must precede the build and the publish; '
    f'order is {names}')

# --- Gap 2: the CE release workflow, injected into the public repo ----------
#
# publish-ce creates the CE TAG and not the CE RELEASE, because the deploy key
# pushes refs and cannot call the releases API. Filed as a P3 on 2026-09-06
# with this exact route identified, and unbuilt for sixteen days — during which
# 2026.9.5 shipped with a tag and no release. A filed P3 with no owner and no
# date is indistinguishable from a step nobody wrote down.
tmpl='scripts/public-ce-templates/publish-release.yml'
assert (root/tmpl).exists(), 'the CE release workflow template must exist'
assert 'publish-release.yml' in (root/'scripts/export-public-ce.sh').read_text(), \
    'the CE export must inject publish-release.yml, or the workflow never reaches the public repo'
pr=read(tmpl)
pron=pr.get('on',{})
# TAG PUSH ONLY. A fork cannot push a tag here and pull_request does not fire
# push:tags, so this reduces the trigger set to holders of tag-push rights. A
# workflow_dispatch would let any write collaborator fire it at an arbitrary
# ref carrying a fabricated section — defensible for an idempotent image build,
# not for a release object.
assert set(pron.keys()) == {'push'}, \
    f'the CE release workflow must trigger on tag push ONLY; found {sorted(pron.keys())}'
assert 'tags' in pron['push'] and 'branches' not in pron['push'], \
    'the CE release workflow must trigger on tags, never on branch pushes'
# EXPLICIT permissions, per job (packaging design amendment 2026-10-02,
# review 20261002-4bfc M1). The workflow grants read; only `release` writes
# contents, and only `binaries` mints attestations. A workflow-level
# `contents: write` would reach every job, including the one holding id-token.
assert pr.get('permissions') == {'contents': 'read'}, \
    f"the CE release workflow's workflow-level permissions must be contents: read; found {pr.get('permissions')}"
jobs=pr['jobs']
for name, job in jobs.items():
    perms=job.get('permissions')
    assert perms is not None, f'publish-release job {name} must declare its own permissions'
    if name == 'release':
        assert perms == {'contents': 'write'}, f'the release job must hold contents: write only; found {perms}'
    elif name == 'binaries':
        assert perms == {'contents': 'read', 'id-token': 'write', 'attestations': 'write'}, \
            f'the binaries job must hold contents: read, id-token: write, attestations: write; found {perms}'
    else:
        assert perms == {'contents': 'read'}, f'publish-release job {name} must hold contents: read only; found {perms}'
# The binaries are tested before they are built (M6) and a Mac runs the
# darwin binary before it is attached (M2).
assert jobs['binaries'].get('needs') == 'test', 'the binaries job must need the test job'
assert 'macos-check' in jobs['release'].get('needs', []), 'the release job must see the macOS check result'
# Only what runs counts: comments and step names cannot carry an assertion
# (review 20261002-0013 F5).
def runs(job):
    out=[]
    for st in job.get('steps',[]):
        for line in str(st.get('run','')).splitlines():
            if not line.strip().startswith('#'):
                out.append(line)
        w=st.get('with') or {}
        out.extend(str(v) for v in w.values())
    return '\n'.join(out)
binsteps=runs(jobs['binaries'])
assert 'darwin' in binsteps and 'dist/vornikctl-*' in binsteps, \
    'the binaries job must build darwin vornikctl and attest every vornikctl binary (M3)'
assert '%cI' in binsteps, 'BuildDate must come from the commit, not the clock, so re-runs build the same bytes (M5)'
relsteps=runs(jobs['release'])
# The headline invariant: the release never waits on the Mac. A flaky macOS
# runner withholds the darwin assets; it must not skip the release (0013 F2).
relif=str(jobs['release'].get('if',''))
assert '!cancelled()' in relif and "needs.binaries.result == 'success'" in relif, \
    f"the release job must run whenever the binaries built; found if: {relif}"
assert 'macos-check' not in relif, \
    f"the release job must not require the macOS check; found if: {relif}"
macsteps=runs(jobs['macos-check'])
assert 'exit 1' in macsteps and 'arch -x86_64' in macsteps, \
    'the macOS check must fail, not skip, when it cannot run the amd64 binary (0013 F3)'
assert 'gh release edit' in relsteps, 'a re-run that attaches the macOS binaries must retract the withheld line (0013 F1)'
assert '--clobber' not in relsteps, 'release uploads must never replace an asset'
assert "grep -qx" in relsteps, 'release uploads must skip assets already on the release'
assert 'MAC_OK' in relsteps and 'vornikctl-darwin' in relsteps, \
    'darwin assets must be uploaded only behind the macos-check result (M2)'
assert 'were not attached' in relsteps, 'a withheld Mac asset must be said in the release notes (M2)'
# The CE ci.yaml carried a v*-tag release job that never ran: CE tags are
# calendar versions. It must not come back beside publish-release.
ceci=read('scripts/public-ce-templates/ci.yaml')
ceon=ceci.get('on', ceci.get(True, {}))
assert 'tags' not in (ceon.get('push') or {}), 'the CE ci.yaml must not trigger on tags; releases are publish-release.yml\'s'
assert 'release' not in ceci['jobs'], 'the CE ci.yaml must not carry a release job'
assert 'macos-check' in ceci['jobs'], 'the CE ci.yaml must run the darwin checks on every push'
assert '--- PASS' in runs(ceci['jobs']['macos-check']), \
    'the CE macOS check must count the tests it ran, or it can pass on none (0013 F4)'
# `vornikctl version` prints to stderr. Piping it to grep matched nothing, and
# the 2026.10.2 release withheld both darwin binaries although they ran
# (packaging design, "As shipped, 2026.10.2"). A version check must capture
# both streams and match the captured text, never pipe into grep -q.
import re as _re
for _wf, _job in (('publish-release.yml', jobs['macos-check']), ('ci.yaml', ceci['jobs']['macos-check'])):
    _r = runs(_job)
    assert not _re.search(r'\bversion\b[^\n]*\|', _r), \
        f'{_wf} macos-check pipes vornikctl version into another command; capture it with 2>&1 and match the text'
    assert _re.search(r'version 2>&1', _r), \
        f'{_wf} macos-check must capture vornikctl version with 2>&1 (it prints to stderr)'
# The tag is matched against the captured text, not some other stream, and
# an empty ref cannot make the match vacuous (review 934d F2, F4). The regex
# guards single-line pipes, the shape that shipped; it does not parse shell.
_pr = runs(jobs['macos-check'])
assert _re.search(r'case "\$out" in\s*\n\s*\*"\$\{GITHUB_REF_NAME\}"\*', _pr), \
    'publish-release macos-check must match ${GITHUB_REF_NAME} against the captured $out'
assert '[ -n "${GITHUB_REF_NAME}" ]' in _pr, 'publish-release macos-check must refuse an empty GITHUB_REF_NAME'
# The export-time check runs the binary it builds, not only compiles it, so
# an execution failure shows on the export push before any tag.
_mac = runs(ceci['jobs']['macos-check'])
assert 'agent connect --help' in _mac, \
    'the CE ci.yaml macos-check must run the built vornikctl (version, agent connect --help)'
# Not vacuous (8a47 F5): the built file is the file that runs.
_b = _re.search(r'go build -o (\S+) \./cmd/vornikctl', _mac)
assert _b and _b.group(1) != '/dev/null', 'the CE macos-check must build vornikctl to a real path'
assert _mac.count(_b.group(1)) >= 3, \
    f'the CE macos-check must run the binary it built ({_b.group(1)}): version and agent connect --help'
prbody=str(pr['jobs'])
assert 'gh release view' in prbody, \
    'creating the CE release must be idempotent — a re-run must not fail on an existing release'
assert 'docs/public/release-notes/index.md' in prbody, \
    'the CE release body must come from the curated PUBLIC notes, never the EE ones'

# The podman lane frees runner disk BEFORE it builds anything, and prints the
# free space on both sides of the lane. Incident 2026-10-05: CI on the 2026.10.4
# release commit (run 37295660864) failed with "no space left on device" while
# rootless podman made an ID-mapped copy of the agent image's layers; the job
# had never measured its disk, so the failure carried no numbers.
_pod = ci['jobs']['test-e2e-podman']['steps']
_names = [s.get('name', '') for s in _pod]
_free = next((i for i, s in enumerate(_pod) if 'rm -rf' in s.get('run', '')), None)
_lane = next((i for i, s in enumerate(_pod) if 'make test-e2e' in s.get('run', '')), None)
assert _free is not None, 'the podman e2e lane must free runner disk (the 2026-10-05 ENOSPC)'
assert _lane is not None and _free < _lane, 'disk must be freed before the podman lane runs'
assert 'df -h' in _pod[_free]['run'], 'the disk step must print free space before and after'
_after = [s for s in _pod[_lane + 1:] if 'df -h' in s.get('run', '')]
assert _after and 'always()' in _after[0].get('if', ''), \
    'free space after the lane must print on failure too (if: always())'
# Never the hosted toolcache as a whole: setup-go's Go lives there.
assert '/opt/hostedtoolcache ' not in _pod[_free]['run'] + ' ', \
    'do not remove the whole hosted toolcache; setup-go installs into it'

print('CI/release policy: PASS')
PY
