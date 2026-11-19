# GitHub App

!!! note "Community Edition"

    The GitHub App integration is part of the free, open-source **Community Edition**, and so of every Enterprise install. See [Editions](../editions.md) for the full Community vs Enterprise matrix.

A GitHub App lets Vornik react to issues and pull requests in the
repositories you choose. There are two ways to set one up: create it from
the console in one step, or connect an App you made yourself.

## Create the App from the console

Open **Integrations → Review pull requests with GitHub** (`/ui/github`).

1. Choose who owns the App: your GitHub account, or an organisation by
   name. For GitHub Enterprise Server, enter its address (for example
   `https://github.example.com`); leave it empty for github.com.
2. Press **Create GitHub App**. GitHub opens with the App filled in: its
   name (`Vornik (<your host>)`, which you may change), permissions and
   events. Press **Create** there.
3. GitHub sends you back to the console, which now shows the App. Press
   **Install on repositories** and pick the repositories.

What the App asks for: Contents, Issues, Pull requests and Checks (read
and write), Metadata (read). It receives issue, issue-comment,
pull-request and pull-request review-comment events. Actions (read), for
CI outcomes, stays a separate grant you add later.

What you need first: the instance's public `https` address set as
`server.public_base_url` (or `auth.external_base_url`). GitHub delivers the
App's events to `<that address>/api/v1/github-app/webhook`, so it must be
reachable from GitHub. The page refuses to start without an `https` address;
it cannot check that GitHub can actually reach it.

What Vornik keeps: the private key as a protected file
(`<config dir>/secrets/github-app.pem`, readable only by Vornik's user), the
webhook secret in the encrypted credential store
(`VORNIK_GITHUB_APP_WEBHOOK_SECRET`), and the App's id, name and owner in
`config.yaml` (`github_app_registration`). You never see or paste a key.
One App per instance: to replace it, press **Forget this App**, which
removes the three, then delete the App on GitHub yourself.

Installing sets up review: each repository you install the App on gets a
review project of its own (`gh-<owner>-<name>`) within seconds, with no
YAML. The GitHub page lists the repositories, with **Pause** and
**Resume**. A repository you remove from the installation keeps its project
and history, and it stops being reviewed. One event sets up at most 50
repositories; the rest wait on the page with **Provision**. These projects
review pull requests and record CI outcomes; they do not answer
`@mentions`.

## Connect an App you created yourself

This integration is **project-scope**: it connects one project. If your
account is scoped to a single project, the hub picks it for you. Create the
App in your organisation's settings first; the creation and webhook steps
are in [Conversation Channels → GitHub](../guides/conversation-channels.md#github).

| Field | Required | Secret | What to enter |
|---|:---:|:---:|---|
| App ID | Yes | No | Shown on your GitHub App's settings page. |
| Installation ID | Yes | No | Shown in the URL after installing the App on your org or repo. |
| Private key | Yes | Yes | Generate a private key on the App's settings page and paste the whole `.pem` file. The hub stores it in a protected file for you. |
| Webhook secret | Yes | Yes | The secret you set on the App's webhook settings. |
| Allowed repositories | Yes | No | Comma-separated `owner/repo` entries this channel accepts events from, e.g. `myorg/myrepo`. |
| API base URL | No | No | For GitHub Enterprise Server only; leave blank for github.com. |

All five required fields go together: an integration with only some of
them set does not activate.

**Test connection** mints a short-lived installation token from the App ID,
installation ID and private key, and reports the installation's account
login. That confirms the App is configured and installed where you expect.
The webhook secret and the repository list are not sent to GitHub by the
test: they decide which inbound events Vornik accepts.

Label matching on issues and pull requests, the sender allowlist and the
rest of the behaviour once connected are in the **GitHub** section of
[Conversation Channels](../guides/conversation-channels.md#github).
