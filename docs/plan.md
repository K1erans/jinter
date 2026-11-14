# Scruple plan

Scruple enforces coding standards that ordinary lint rules can't express,
such as "don't call the payment API outside billing". Each rule is a
plain-language question answered by TypeSafe's Jev model, run against diffs
in CI and against whole repos on request.

Status: draft, 2026-10-10. Items marked **Open** are still to be decided.

## 0. Users and onboarding

Scruple serves three kinds of user. At sign-up, each user **chooses** the
onboarding path that fits them, and can switch later.

| Path | Who | Job | What they do today | Entry point | Needs most |
|---|---|---|---|---|---|
| **Team** | Tech lead / staff engineer, team of 5–50 | Stop the architecture drift they keep re-explaining in review | Review comments, docs nobody reads | Install the GitHub app | Shadow mode, baseline, check-run annotations |
| **Solo** | Solo dev / small team, 1–5 people | Keep AI-written code on their own conventions | CLAUDE.md, memory, re-prompting | The open-source CLI | Cheap, local, no account needed |
| **Org** | Platform / security team, many repos | Enforce compliance rules across 100s of repos | Semgrep, custom scripts, audits | Org-wide GitHub app install | Org-wide rules, SSO, audit log, roles |

**All three paths ship in v1.** So v1 also includes the Org needs:
org-wide rules, SSO, an audit log and roles.

### Team onboarding

1. Install the GitHub app.
2. Pick repos.
3. Pick rules from **templates** (layering, secrets, error handling, …) and/or
   **write custom rules**.
4. Scruple runs a **shadow scan** of the whole repo and shows the results
   ("3 rules · 12 hits") without blocking anything.
   Scruple can also **suggest rules mined from past PR review comments**
   ("you've said this 9 times"), which the user can accept, edit or ignore.
   This needs read access to PR comments.
5. Whenever they choose, the user reviews hits (✔ real / ✘ false), which
   gives each rule an accuracy figure, then enforces the rule. Its current
   real hits become its baseline. The user can **re-run the scan manually**
   at any point, for example after tuning a rule.

When a rule has **zero hits**, its results show how much was checked
("checked 4,210 functions") and the three closest matches below the
threshold, with [lower threshold] and [edit rule] actions.

While a scan runs, hits appear per rule as they are found, with overall
progress shown. The user can leave the page and is emailed when the scan
finishes.

### Solo onboarding

- The CLI pays for Jev calls in one of two ways: it uses `TYPESAFE_API_KEY`
  if set (fully standalone), otherwise the user's Scruple login (free tier,
  routed through the Scruple server). No account is needed if the user brings
  their own key.
- `scruple init` offers an interactive template picker (space to toggle, plus
  "write my own"), writes `.scruple.yaml`, then asks "Run a check now?".
  Templates live in the public repo and are shared with the Team path.
- The **first** `scruple check` (right after init) scans the **whole repo**.
  After that, `scruple check` scans the **branch diff** (`main...HEAD`), which
  is what CI checks on the PR. `scruple check --all` rescans the whole repo.

### Org onboarding

- Sign up with GitHub → install the app org-wide → rules and pilots. **SSO
  comes later**, under Settings → SSO, with a "require SSO" toggle (audited).
- New org-wide rules roll out to **pilot repos first**: create the org rule →
  pick pilot repos → shadow → review → enforce on the pilots → expand to more
  repos in stages.

Steps for the other paths are still to be settled in grilling (see section 7).

### Orgs and accounts

- **Every account has a personal org** (e.g. "Kieran's org"), created at
  sign-up. Solo users work in it.
- A user can belong to several orgs (their personal org, Acme, …) and switch
  between them. Rules, repos, tokens and billing all belong to an org.
- **Joining an org:** an org is linked to its GitHub org. Members of that
  GitHub org see "Join Acme" and join as Member in one click, and they lose
  access when they're removed from the GitHub org. **Email invites** cover
  contractors and people without GitHub.

### Repos, projects and sections

- **Project** = a top-level section inside a repo (e.g. `web-app` in
  `acme/platform`). Projects are what users work in day to day.
- The web app **opens on a project list**, not a dashboard. Each project has
  its own dashboard, scoped to the rules that apply there (including
  inherited org and repo rules).


- A repo in Scruple is one GitHub repo. Inside it, a **tree of sections of
  any depth**, each covering a set of paths:

  ```
  acme/platform                 repo
  └─ web-app                    section  apps/web/**            rules: R1  admins: X
     └─ api                     section  apps/web/api/**        rules: R2
        └─ payment-api          section  apps/web/api/payments/**  rules: R3  admins: Y
  ```
- Rules attach to the org, the repo or any section. They apply there and to
  everything below it, so payment-api gets R1+R2+R3 (plus org and repo rules).
- A lower level can **tune** an inherited rule but can't turn it off. Locked
  rules can't be tuned. This is the same model as org rules on repos.
- Admins can be assigned to any section and manage it and its children.

### Sign-in (all v1)

- GitHub OAuth (everyday sign-in).
- Google and email magic link, for dashboard-only users.
- Enterprise SSO through SAML 2.0 and OIDC (Okta, Entra ID, Google Workspace,
  OneLogin, JumpCloud, Ping, …).
- SCIM provisioning and deprovisioning.
- GitHub Enterprise Managed Users work through GitHub sign-in.

## 1. Parts and repos

| Part | Repo | Notes |
|---|---|---|
| Engine (walk, chunk, jev, cache, search) | public | This repo, still named `jinter`; to be renamed `scruple` |
| Rule format (`rules.Rule`, YAML loader, JSON Schema, rule → `Query`, diff scoping) | public | Needed by the CLI, so it lives here |
| CLI (`scruple`) | public | Open source |
| Server: GitHub app webhooks + web API + scan jobs | private | One Go server to start. A separate scan worker comes only when scans get too long for webhooks |
| Web app | private | Talks to the server's API |
| Database | private | Postgres |

The private repo imports the public Go module. Engine packages must not
depend on anything product-specific.

Layout of the public repo:

```
scruple/
  go.mod
  scruple.go, walk/, chunk/, jev/, cache/, search/   # engine
  rules/                                            # rule format, loader, schema, rule → Query
  cmd/scruple/                                      # CLI
```

## 2. Where rules live

- **The web app (its database) is the only source of truth for the GitHub app.**
  The GitHub app never reads rule files from repos.
- The CLI runs locally from `.scruple.yaml`. It never needs an account to run
  `scruple check`.
- Rules move between the two only through explicit CLI commands (section 3).
  Otherwise, rules are created in the web app.

### Rule shape (draft)

```yaml
rules:
  - id: payments-only-in-billing      # stable, shared by file and server
    question: "calls the payment provider's API"
    allow_paths: [src/billing/**]
    threshold: 0.7
    severity: error                   # error | warning | info
```

- In the file: YAML, because people write and review it by hand.
- In the DB: normal columns, with paths as `text[]` or a child table. Not a
  JSON blob. `jsonb` is acceptable only for small optional extras.
- One Go struct (`rules.Rule`) is the single definition. JSON Schema
  generated from it validates both the YAML file and the web form.

### Rule history

- Rules are never overwritten. Each edit, whether from the web or a CLI push,
  adds a row to `rule_versions` (rule id, version, contents, author, time,
  source: web/cli). `rules` points to the current version.
- Restoring an old version copies it forward as a new version.
- **Each scan run records the exact `rule_version` ids it used**, so old
  results can always be explained with the rule as it was at the time.

## 3. CLI

```
scruple check                          # run local .scruple.yaml rules against the code
scruple login                          # device flow (see section 4)
scruple rules diff --repo acme/api     # local file vs server: added / changed / only-on-server
scruple rules pull --repo acme/api     # write server rules into .scruple.yaml (shows diff, confirms)
scruple rules push --repo acme/api     # add/update rules on server (shows diff, confirms)
scruple hook install --repo acme/api   # pre-push hook running check on the outgoing diff
```

- `--repo` is **always required** for commands that touch the server. It is
  never inferred from the git remote.
- `push` only adds or updates. It never deletes server rules that are missing
  from the file. A later `--prune` flag could delete them explicitly.
- `push` and `pull` show the diff and ask for confirmation. `--yes` skips the
  prompt for scripts.
- Pushing over an existing rule creates a new version, so nothing is lost.

## 4. Auth

- The web app signs in with GitHub OAuth.
- `scruple login` uses the OAuth device flow: it prints a code and opens the
  browser, the user approves in the web app, and the CLI receives a Scruple
  token. It works over SSH and in containers.
- API tokens are created in the web app for CI and scripts. Each is named,
  limited to one org, and revocable. `SCRUPLE_TOKEN` overrides any stored
  token.
- The CLI gets a **Scruple** token, never a GitHub token.
- Tokens are kept in the OS keychain, with a file fallback.
- **Open:** build auth in-house or use a hosted provider. With SAML, OIDC,
  SCIM, magic links and the device flow all in v1, a hosted provider is
  strongly favoured. Needs research on options and cost.

## 5. CI/CD behaviour

Priority: **v1** = first release, **later** = after v1.

### Noise (adoption depends on this)

| Item | Priority | Summary |
|---|---|---|
| Baseline | v1 | The first full scan after a rule is added records existing violations. PRs fail only on new ones |
| Shadow mode | v1 | New rules run and record results without blocking or annotating. The owner tunes them, then switches them to blocking |
| Severity → outcome | v1 | `error` fails the check, `warning` annotates only, `info` shows only in the web app |
| Dismiss false positives | later | A "not a violation" button on the check run, or `/scruple dismiss <reason>`. Recorded per rule version. Each rule shows its false-positive rate. Dismissals stay in place while the code is unchanged |
| Inline suppressions | later | `// scruple:ignore <rule-id>: <reason>`. A reason is required. The web app lists every suppression |

### Speed and cost

| Item | Priority | Summary |
|---|---|---|
| Diff-only PR scans | v1 | Full scans run on a schedule, when a rule changes, or on request |
| Answer reuse | v1 | Answers are cached by chunk content hash + rule version, in a server-side cache shared by all PRs of a repo. New pushes only check changed chunks. Re-runs are free and give the same result |
| Cancel superseded scans | v1 | A new push to a PR cancels the scan still running for the old commit |
| Skip drafts | later | Scans run on `ready_for_review`. A `scruple:skip` label skips a PR |
| Budgets | later | Per-repo caps on chunks or calls per PR and per month. If a PR goes over, the check reports which files it covered |

### Reliability

| Item | Priority | Summary |
|---|---|---|
| Outage policy | v1 | Per-repo setting for whether PRs pass or are blocked when TypeSafe/Jev is down. The default lets them through with a neutral status and retries automatically |
| Re-run button | v1 | Requested action on the check run |
| Merge queue | later | Handle `merge_group` events so Scruple can be a required check |

### Developer experience

| Item | Priority | Summary |
|---|---|---|
| Check run + PR comments | v1 | Check run for pass/fail, a summary comment and inline review threads (see Fixing violations). Each thread shows the rule, version, probability and a link to the rule |
| Pre-push hook | v1 | `scruple hook install`, using pulled rules |
| GitHub Action | v1 | Wraps the CLI with `SCRUPLE_TOKEN` for orgs that can't install GitHub apps |

### Fixing violations

Each violation (on a PR or in the web app) offers ways to get it fixed:

- **PR comments (always on), Greptile-style:**
  - One **summary comment** per PR, edited in place on each push: the
    violations, what was fixed since the last push, and a "Fix all with ▾"
    action.
  - One **inline review thread** per violation, on the offending line: rule,
    probability, [Copy prompt] and [Fix with ▾]. People can 👍/👎 and reply,
    and that feeds the rule's false-positive rate. The thread resolves itself
    once the code is fixed.
  - The **check run** only reports pass/fail, for required checks.
- **Fix prompt:** a ready-made LLM prompt (rule, offending code, what to
  change) the user can copy into their own coding agent.
- **Hand off to the repo's agent:** a "Fix with ▾ Claude | Copilot | Codex"
  action posts the fix prompt as a comment that mentions the agent (e.g.
  `@claude fix: …`) or assigns the agent. The repo's existing agent makes the
  fix with its own permissions and billing. Scruple never writes code, needs
  no write access to code, and doesn't pay for fix tokens.

### Org

| Item | Priority | Summary |
|---|---|---|
| Org-wide rules | v1 | Rules defined once for the org and applied across its repos |
| Repo overrides | v1 | A repo can tune an org rule (severity, threshold, paths) but can't turn it off. The org can **lock** individual rules so repos can't change them at all. Every override goes in the audit log |
| SSO | v1 | Org sign-in through the org's identity provider |
| Audit log | v1 | Who changed which rule, token or setting, and when |
| Roles | v1 | Four fixed roles. **Org admin**: billing, SSO, members, locking rules, tokens. **Rule author**: create/edit org rules and roll them out. **Repo admin**: tune org rules on their repos, repo rules, enforce; assigned automatically from GitHub admin/maintain permission. **Member**: view, 👍/👎, fix actions. Custom roles are later |

### Trust and privacy

| Item | Priority | Summary |
|---|---|---|
| Path exclusions | v1 | Per-repo paths that are never sent to TypeSafe |
| Sent-code record | later | The web app shows exactly which code was sent for each run |

## 6. Engine work this implies

- Diff-scoped search: take a base/head pair and search only the changed chunks.
- A cache interface the server can back with Postgres or Redis, instead of
  local SQLite.
- Cache keys that include the rule version.
- The rule → `Query` mapping, including `allow_paths` and the threshold.
- Inline suppression parsing (later).

## 7. Open questions for grilling

- **Product:** What does "repo" mean in the web app (GitHub repo, or a project
  covering several)? Are there org-wide rules, and can a repo opt out? Who can
  edit rules (roles)? What does a user see on first install, before any rules
  exist? What does a rule author see in shadow mode?
- **Architecture:** Where do scans run, and what are the timeout limits? How is
  the shared cache keyed and isolated between tenants? How does the server get
  the code (clone vs. API)? How are webhook retries kept idempotent?
- **Program design:** What exactly is in `rules.Rule`? What does the API
  contract for push/pull/diff look like? Where does the seam sit between the
  public `rules` package and the private store?
- **Edge cases:** A rule is renamed (id change) between push and pull. A rule
  is edited mid-scan. A PR touches thousands of files. A force-push rewrites a
  scanned commit. A baseline is taken on a branch that is later deleted.
  Should a dismissal carry over when the code moves?
- **Business:** pricing (per seat, per repo, per scan), the free tier, and what
  stays in the open-source CLI versus paid features.
