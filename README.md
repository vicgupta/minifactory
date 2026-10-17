# minifactory-go

A minimal software factory on one VPS: a queue, a single Go binary, and Docker.
No web server, no UI, no webhook listener — GitHub is the UI.

It runs as an unprivileged user — never root — with its own queue
(`data/queue.json`), worker image (`factory-worker-go`), and a timer firing
every minute: a systemd timer whose service runs as the `minifactory` user,
or, for users without root, a per-user crontab entry managed by
`minifactory install-cron` (see "Run without root").

```
GitHub issue (label: ready) ──poll──▶ queue (data/queue.json)
                                            │
minifactory run-once (every 1 min via systemd timer or cron)
  1. shallow-clone repo on host → work/<id>
  2. agent works inside a disposable Docker sandbox (full egress,
     2 CPU / 4 GB): the stub or a real agent CLI (claude/codex/opencode),
     with only the task workdir mounted and only that agent's token
     injected. Without docker it falls back to the host with a warning.
  3. host validates result.json {changed_files, tests_passed, summary},
     cross-checks changed_files against git status, and for real agents
     re-runs the test suite inside a sandbox
  4. commit + push branch factory/<id>, open PR via GitHub API
                                            │
                                     state: pr_open
                                            │
                    ◀── HUMAN GATE: review & merge the PR in GitHub ──▶
                                            │
minifactory sync → merged = done / closed unmerged = failed
```

## Install

No Go toolchain needed. Grab the tarball for your platform from the
[releases page](https://github.com/vicgupta/minifactory/releases) —
`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64` — and unpack
the `minifactory` binary onto your PATH:

```bash
tar -xzf minifactory-0.0.21-linux-amd64.tar.gz
cp minifactory ~/.local/bin/   # or /usr/local/bin
```

Verify the download against `sha256sums.txt` from the same release.

## Update

```bash
minifactory update          # upgrade to the latest release in place
minifactory update --check  # just report whether an update exists
```

`update` downloads the tarball for the current platform, verifies its
SHA256 against the release's `sha256sums.txt`, smoke-tests the new binary,
then atomically swaps it in — keeping the previous binary as
`minifactory.bak` and rolling back automatically if anything fails.

## Configuration

`<factory-dir>/.env` (mode 600) is the **only** env file. Copy
`.env.example` (or run `minifactory init`) to create it. The knobs:

- `GITHUB_TOKEN` — required. Clone/push/open PRs on github.com. Without it,
  only local `file://` repos work and PRs must be opened manually.
- `CLAUDE_CODE_OAUTH_TOKEN` — optional. Enables the claude runner (`claude -p`
  on the host). This is a Claude subscription token, not an API key: run
  `claude setup-token` on a machine logged into your Claude Pro/Max plan and
  paste the output here. The `claude` CLI must be installed on the host.
- `CODEX_TOKEN` — optional. Enables the codex runner (`codex exec` on the
  host). Must be a real OpenAI API key. codex-cli ignores `OPENAI_API_KEY`
  on its own, so the factory registers the key once via
  `codex login --with-api-key` (stored in `~/.codex/auth.json`, mode 600;
  skipped when the stored key already matches). The `codex` CLI must be
  installed.
- `OPENCODE_TOKEN` — optional. Enables the opencode runner (`opencode run`
  on the host, token passed as `OPENCODE_API_KEY`). The `opencode` CLI must
  be installed.
- `MAX_TURNS` — optional. Turn budget per task for the claude runner
  (default 30). Raise for large tasks; each turn costs model usage.
- `AGENT` — optional. Force which runner handles tasks:
  `claude` | `codex` | `opencode` | `stub`. The chosen runner's token must
  be set (stub needs none); otherwise the factory warns and falls back.
- `MAX_COST_USD_PER_TASK` — optional. Per-task spend cap in USD (e.g. 2.50).
  When the sizer's or the agent's reported model cost exceeds it, the run
  record is flagged `over_budget`, the PR body carries a warning, and
  `minifactory report` lists it. Runs are never killed for going over
  budget. Unset = off. (On a Claude subscription token the reported cost
  is an API-equivalent cost, not a real bill.)

Priority when several agent tokens are set and `AGENT` is unset:
claude > codex > opencode. With none set, the deterministic stub runs — it makes a real code change + test,
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

- The factory never runs as root. The systemd service runs as the
  unprivileged `minifactory` user with hardening (`NoNewPrivileges`,
  `ProtectSystem=strict`, `PrivateTmp`); the cron path runs as whichever
  user installed it.
- Sandbox containers get full egress (agents need package registries) plus
  cpu/mem/pids caps and `--rm`. They are disposable: one per task.
- **Agents run inside a disposable Docker sandbox.** The worker image
  (`factory-worker-go`) carries the agent CLIs alongside the stub. Each run
  mounts only its task workdir and injects only that agent's own token —
  via a 0600 env-file staged outside the repo and removed afterwards, so it
  never appears in docker argv, `ps` output, the git history, or the task
  prompt (which travels via a read-only mounted file). `GITHUB_TOKEN`, the
  other agents' tokens, and SSH/cloud credentials never enter a container.
  Rebuild the image after pulling new CLI versions:
  `docker build -t factory-worker-go worker/`. Without docker or the image,
  the CLI falls back to the host with a scrubbed env and a loud warning.
- **Agent subprocesses get a scrubbed environment.** They inherit only a
  whitelist (`PATH`, `HOME`, proxy vars, …) plus their own runner's token.
  `GITHUB_TOKEN`, the other agents' tokens, and SSH/cloud credentials are
  dropped — an agent executing untrusted task text cannot exfiltrate them.
  Orchestrator git/API calls keep the full env they need.
- Agent CLIs run fully non-interactive (opencode `--auto`, claude
  `--dangerously-skip-permissions`, codex
  `--dangerously-bypass-approvals-and-sandbox`) with stdin closed, so a
  permission prompt can never hang a cycle.
- The typed handoff is verified, not trusted: `changed_files` is
  cross-checked against `git status` (a dishonest handoff fails the task),
  and for real-agent tasks the runner re-runs the test suite inside a
  disposable sandbox, so generated code executes only there — never on the
  host.
- The agent can only write its task workdir and push its own `factory/<id>`
  branch. Nothing merges without human review of the PR in GitHub.

## Run without root

No root, no systemd unit? Put the binary on your PATH (`~/.local/bin`),
point it at a writable dir, and let `install-cron` manage a per-user
crontab entry:

```bash
cp minifactory ~/.local/bin/
minifactory init --dir ~/.minifactory
minifactory install-cron --dir ~/.minifactory
```

This writes a marked crontab block that runs one `run-once` cycle every
minute, guarded by `flock` (cron has no overlap protection of its own —
two concurrent cycles would race on `data/queue.json`), with
`MINIFACTORY_DIR` set. Useful flags: `--schedule "*/5 * * * *"` for a
different cadence, `--force` to replace an existing entry, `--remove` to
take it back out. Re-running `install-cron` without flags is a safe no-op
when an entry already exists.

Caveats vs the systemd path: no `ProtectSystem`/`NoNewPrivileges`
hardening (those need a system unit), and the docker sandbox needs docker
group membership. Everything else — queue, tokens, agent runners, the
scrubbed agent env — works the same.

## Operate

```bash
cd /home/minifactory   # the factory dir (or ~/.minifactory for a rootless setup)
./minifactory issue --repo https://github.com/ORG/REPO.git --title "Fix X" --body "..."
# or --body-file path/to/spec.md, or --body-file - to read the body from stdin
./minifactory list [--state pr_open,failed]  # optionally filter to states
./minifactory logs <id>
./minifactory run-once   # normally the systemd timer does this
./minifactory sync
./minifactory poll
./minifactory retry <id> [--force] # reclaim a stuck/failed task: re-queues
                         # failed or crashed (running) tasks; retries just
                         # the PR creation for pr_open tasks with no PR URL
./minifactory report [--since 30d] [--task <id>] [--json]
                         # run records: success rate, time to PR, slowest
                         # stage, costs per agent, most expensive tasks;
                         # --task shows every attempt stage by stage
./minifactory init       # (re)initialize this dir: runtime dirs, queue reset
                         # (existing data/queue.json backed up to
                         # queue.json.bak.<timestamp>), fresh .env
                         # (existing .env backed up to .env.bak.<timestamp>),
                         # factory issue labels ensured, then a config check.
                         # Refuses when .env is configured or the queue is
                         # non-empty unless --force is given.
./minifactory init --force  # same, but proceed past live state (backups still made)
./minifactory init --dir /srv/factory2  # initialize a brand-new factory dir
./minifactory install-cron [--dir path]  # manage a per-user crontab entry:
                         # one flock-guarded run-once per minute (see
                         # "Run without root"); --schedule, --remove, --force
./minifactory update [--check]  # self-update to the latest GitHub release
                         # (checksum-verified, smoke-tested, atomic swap
                         # with rollback — no Go toolchain needed)
./minifactory delete-all [--dir path]  # tear down an installation: systemd
                         # units, crontab entry, the nologin service user,
                         # and the dir's managed state (prints a plan first;
                         # --force executes; binary is left in place)
./minifactory version    # print the factory version
```

Add `--log` before or after any command to narrate every step on stdout:

```bash
./minifactory --log run-once   # watch the pipeline: queue pick, clone, sizing,
                               # agent, tests, push, PR — each step timestamped
./minifactory poll --log        # watch the GitHub issue scan and queueing
```

`--log` output is human-readable; don't script against it (`issue` keeps
printing just the task id as its last line).

## Versioning

The version lives in `version.go` (semver, starting at 0.0.1). **Bump the
patch number on every code change, before rebuilding:**

```bash
./scripts/bump-version.sh  # 0.0.1 -> 0.0.2
go build -o minifactory .
```

`minifactory version` and `minifactory doctor` (including `--json`) report the
running version, so you can always tell which build is deployed.

`doctor` sanity-checks token shapes (a stub where a real token belongs is a
warning — this is what a dead credential looks like before it 401s), verifies
the worker image carries the selected agent's CLI (a missing CLI fails the
docker check, since the factory would silently fall back to host execution),
and `doctor --probe` spends one real API call verifying the selected agent's
token is actually accepted. Without `--probe`, doctor stays fast and free:
`--version` checks never authenticate.

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
- `cron.go` — the `install-cron` subcommand: per-user crontab management for rootless scheduling
- `worker/stub.go` — deterministic stub agent, compiled into the worker image
- `worker/Dockerfile` — multi-stage: builds the stub, ships it in the
  python:3.12-slim + git + node + pytest toolchain image (`factory-worker-go`),
  plus the vendor agent CLIs (claude, codex, opencode) for sandboxed runs

## What's deliberately missing

vs the earlier full scaffold (backed up at `/root/factory.bak` on the VPS):
no web dashboard, no push webhooks (issues are polled every 1 min), one task
per cycle, no auth layer. Run records (`data/runs/<id>/<attempt>.json`) keep
per-stage timings and model costs; `minifactory report` aggregates them, and
`MAX_COST_USD_PER_TASK` flags over-budget runs without killing them. The part
that matters — one isolated sandbox per agent, typed result handoff, human
merges every PR — is all here.

## Swap the sandbox later

`dockerBase()` in `sandbox.go` is the seam: replace the Docker invocation with
an Upstash Box / E2B / Daytona call. Everything upstream (queue, validation,
PR creation) stays the same.
