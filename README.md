# minifactory-go

A minimal software factory on one VPS: a queue, a single Go binary, and Docker.
No web server, no UI, no webhook listener — GitHub is the UI.

It lives at `/root/minifactory-go/` with its own queue (`data/queue.json`),
worker image (`factory-worker-go`), and systemd timer (`minifactory-go.timer`,
firing every minute).

```
GitHub issue (label: ready) ──poll──▶ queue (data/queue.json)
                                            │
minifactory run-once (every 5 min via systemd timer)
  1. shallow-clone repo on host → work/<id>
  2. agent works: stub runs in a disposable Docker sandbox (full egress,
     2 CPU / 4 GB), or a real agent CLI (claude/codex/opencode) runs on the
     host against work/<id>
  3. host validates result.json {changed_files, tests_passed, summary};
     for real agents the test suite is re-run inside a sandbox
  4. commit + push branch factory/<id>, open PR via GitHub API
                                            │
                                     state: pr_open
                                            │
                    ◀── HUMAN GATE: review & merge the PR in GitHub ──▶
                                            │
minifactory sync → merged = done / closed unmerged = failed
```

## Configuration

`/root/minifactory-go/.env` (mode 600) is the **only** env file. Copy
`.env.example` (or run `minifactory init`) to create it. The knobs:

- `GITHUB_TOKEN` — required. Clone/push/open PRs on github.com. Without it,
  only local `file://` repos work and PRs must be opened manually.
- `CLAUDE_CODE_OAUTH_TOKEN` — optional. Enables the claude runner (`claude -p`
  on the host). This is a Claude subscription token, not an API key: run
  `claude setup-token` on a machine logged into your Claude Pro/Max plan and
  paste the output here. The `claude` CLI must be installed on the host.
- `CODEX_TOKEN` — optional. Enables the codex runner (`codex exec` on the
  host, token passed as `OPENAI_API_KEY`). The `codex` CLI must be installed.
- `OPENCODE_TOKEN` — optional. Enables the opencode runner (`opencode run`
  on the host, token passed as `OPENCODE_API_KEY`). The `opencode` CLI must
  be installed.
- `MAX_TURNS` — optional. Turn budget per task for the claude runner
  (default 30). Raise for large tasks; each turn costs model usage.

Priority when several agent tokens are set: claude > codex > opencode. With
none set, the deterministic stub runs — it makes a real code change + test,
so the pipeline stays exercisable end to end without any token.

(`GITHUB_REPO=owner/repo` is an optional extra for `minifactory poll`, which
enqueues open issues labeled `ready`.)

Run `minifactory init` to create the runtime directories, reset `data/queue.json`
(an existing queue is backed up to `data/queue.json.bak.<timestamp>` first),
and write a fresh `.env` template in the factory's directory (an existing
`.env` is backed up to `.env.bak.<timestamp>` first, never overwritten in
place). As a safety guard, `init` refuses to run when the target dir already
has a configured `.env` (any credential value set) or a non-empty queue —
re-run with `--force` to proceed (backups are still made). It also ensures the factory issue labels (`factory-ready` /
`factory-inProgress` / `factory-completed` / `factory-createdPR` /
`completed` / `factory-rejected`) exist on the configured `GITHUB_REPO`,
using the pre-existing configuration. `init --dir <path>` sets up a
brand-new factory directory elsewhere.

## Security model

- Sandbox containers get full egress (agents need package registries) plus
  cpu/mem/pids caps and `--rm`. They are disposable: one per task.
- **No secrets ever enter a container.** Tokens live only in `.env` (600)
  and are used host-side (git push, GitHub API, agent CLIs). They are never
  logged.
- Agent CLIs run on the host as trusted tooling. For real-agent tasks the
  runner re-runs the test suite inside a disposable sandbox, so generated
  code executes only there — never on the host.
- The agent can only write its task workdir and push its own `factory/<id>`
  branch. Nothing merges without human review of the PR in GitHub.

## Operate

```bash
cd /root/minifactory-go
./minifactory issue --repo https://github.com/ORG/REPO.git --title "Fix X" --body "..."
./minifactory list
./minifactory logs <id>
./minifactory run-once   # normally the systemd timer does this
./minifactory sync
./minifactory poll
./minifactory retry <id> [--force] # reclaim a stuck/failed task: re-queues
                         # failed or crashed (running) tasks; retries just
                         # the PR creation for pr_open tasks with no PR URL
./minifactory init       # (re)initialize this dir: runtime dirs, queue reset
                         # (existing data/queue.json backed up to
                         # queue.json.bak.<timestamp>), fresh .env
                         # (existing .env backed up to .env.bak.<timestamp>),
                         # factory issue labels ensured, then a config check.
                         # Refuses when .env is configured or the queue is
                         # non-empty unless --force is given.
./minifactory init --force  # same, but proceed past live state (backups still made)
./minifactory init --dir /srv/factory2  # initialize a brand-new factory dir
./minifactory version    # print the factory version
```

## Versioning

The version lives in `version.go` (semver, starting at 0.0.1). **Bump the
patch number on every code change, before rebuilding:**

```bash
./scripts/bump-version.sh  # 0.0.1 -> 0.0.2
go build -o minifactory .
```

`minifactory version` and `minifactory doctor` (including `--json`) report the
running version, so you can always tell which build is deployed.

Watch PRs with `gh pr list` (or the GitHub web UI) and merge there — that merge
is the human gate. `sync` picks up the merge and marks the task `done`.

## The factory label lifecycle

Issues move through five factory labels, and `poll` only picks up
`factory-ready`:

1. `minifactory init` ensures the label set exists on the configured repo
   (`GITHUB_REPO`), creating any that are missing:
   `factory-ready` → `factory-inProgress` → `factory-completed` →
   `factory-createdPR` → `completed` (or `factory-rejected` if the PR is
   closed unmerged).
6. **Automatic sizing.** Before the agent runs, a short sizing pass (claude,
   max 5 turns) judges whether the task fits the agent's turn budget
   (`MAX_TURNS`). If not, the factory creates one GitHub sub-issue per
   sub-task — each labeled `factory-ready` and picked up by the next `poll`
   as an independent task — comments the plan on the parent issue, and
   marks the parent task `split`. `sync` watches the sub-issues and marks
   the parent `done` (issue labeled `completed`) once all are closed. The
   sizing pass is advisory and fail-open: any error or invalid output just
   runs the task normally, and it is skipped entirely for non-claude
   agents.
2. Label any issue `factory-ready`. The next `poll` enqueues it (deduplicated by issue
   number, so re-polling and title edits are safe). `minifactory issue` also
   creates the issue for you when you don't pass `--issue N`.
3. The runner comments on the issue as it goes: picked up → done (with the PR
   link) or failed (with the reason). Comments are best-effort — a comment
   failure never fails the task. On pickup it swaps the issue's
   `factory-ready` label for `factory-inProgress`; any other labels are kept.
4. When the agent's work passes sandbox tests and the branch is pushed, the
   issue's `factory-inProgress` label is swapped for `factory-completed`.
5. When the PR opens, the label becomes `factory-createdPR` — the human gate.
   The PR body ends with `Closes #N`, so merging auto-closes the issue, and
   the next `sync` marks the task done and swaps the issue's
   `factory-createdPR` label for `completed`. If the PR is closed without
   merging, `sync` marks the task failed and swaps the label for
   `factory-rejected` instead.

`GITHUB_TOKEN` needs these scopes on the repo: Contents (read/write, for
clone/push), Pull requests (write, to open PRs), Issues (read for poll, write
for status comments and labels).

## Layout

- `main.go` — CLI dispatch, `.env` parsing, command runner with timeouts, logging
- `store.go` — JSON task store at `data/queue.json` (flock-guarded)
- `agent.go` — token-driven agent selection + vendor CLI runners
- `sandbox.go` — sandbox invocation, test-command detection, sandbox test re-run
- `github.go` — GitHub API, repo URL parsing, token-based push, PR creation
- `pipeline.go` — the task subcommands (issue/run-once/sync/list/logs/poll/retry)
- `doctor.go` — the `doctor` health-analysis subcommand
- `init.go` — the `init` subcommand: runtime dirs, `.env` backup + fresh template, config check
- `worker/stub.go` — deterministic stub agent, compiled into the worker image
- `worker/Dockerfile` — multi-stage: builds the stub, ships it in the
  python:3.12-slim + git + node + pytest toolchain image (`factory-worker-go`)

## What's deliberately missing

vs the earlier full scaffold (backed up at `/root/factory.bak` on the VPS):
no web dashboard, no push webhooks (issues are polled every 5 min), one task
per cycle, no cost accounting, no auth layer. The part that matters — one
isolated sandbox per agent, typed result handoff, human merges every PR — is
all here.

## Swap the sandbox later

`dockerBase()` in `sandbox.go` is the seam: replace the Docker invocation with
an Upstash Box / E2B / Daytona call. Everything upstream (queue, validation,
PR creation) stays the same.
