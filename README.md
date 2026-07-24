# Agent Relay

[![CI](https://github.com/sovereign313/Agent-Relay/actions/workflows/ci.yml/badge.svg)](https://github.com/sovereign313/Agent-Relay/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/Go-1.23%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Agent Relay lets an authorized Telegram user run Codex, Claude Code, OpenCode,
or Grok Build against Git repositories on a Linux PC. Each Telegram chat,
project, and agent gets its own durable session, so switching tools or projects
and returning later resumes the correct conversation.

Agent Relay uses Telegram long polling. It does not expose an inbound port and
does not scrape a TUI. Each agent runs through its supported headless JSON
interface and explicit session-resume flag. Telegram updates enter a durable
local inbox before an agent starts. Completed
responses move atomically into a durable outbox and remain there until Telegram
accepts delivery.

## Security

This application is remote access to coding agents. With `full_access = true`,
Agent Relay uses each CLI's approval-bypass mode:

```text
Codex:    --dangerously-bypass-approvals-and-sandbox
Claude:   --dangerously-skip-permissions
OpenCode: --dangerously-skip-permissions
Grok:     --always-approve
```

The selected agent can therefore read, modify, execute, and access anything
available to the OS user running Agent Relay. The configured project roots
restrict which working directory a Telegram user may select; they are not an
execution sandbox. Treat the Telegram bot token and every allowed Telegram
account like an SSH key.

For stronger isolation, run Agent Relay as a dedicated OS user or inside a
container with only the intended project tree mounted writable. Use private
Telegram chats, keep the allowlist short, and protect the configuration, agent
credentials, state file, and logs with restrictive permissions.

## Requirements

- Linux
- Go 1.23 or newer
- At least one supported CLI installed and authenticated:
  [Codex](https://developers.openai.com/codex/cli/),
  [Claude Code](https://code.claude.com/docs/en/cli-usage),
  [OpenCode](https://opencode.ai/docs/cli/), or
  [Grok Build](https://docs.x.ai/build/overview)
- Git repositories beneath one or more configured project roots
- A Telegram bot token

Verify the CLIs you plan to enable before starting:

```sh
codex --version
claude --version
opencode --version
grok version
```

## Telegram Setup

1. Open a chat with `@BotFather`.
2. Run `/newbot`, follow the prompts, and retain the bot token.
3. Find your numeric Telegram user ID using a trusted ID bot or by calling the
   Telegram `getUpdates` API after messaging your new bot.
4. Put only that numeric ID in `allowed_user_ids`.
5. Start a private chat with the new bot and send `/start`.

Agent Relay logs unauthorized user and non-private-chat attempts without logging
message bodies or the bot token.

## Configuration

Copy `config.example.toml` to `config.toml` and update the token, user ID, roots,
and optional aliases:

```toml
telegram_token = "123456:replace-me"
allowed_user_ids = [123456789]
private_chats_only = true

project_roots = ["/mnt/external/projects"]
project_discovery_depth = 5
default_agent = "codex"
state_file = "./var/state.json"
log_file = "./var/agent-relay.log"
log_max_bytes = 10485760
log_backups = 3
queue_size = 5
max_message_bytes = 32768
task_timeout = "2h"

[project_aliases]
harness-studio = "WaspLogic/HarnessStudio"
rapidrfq = "WaspLogic/RapidRFQ"

[agents.codex]
type = "codex"
command = "codex"
args = []
full_access = true

[agents.claude]
type = "claude"
command = "claude"
args = []
full_access = true
enabled = false

[agents.opencode]
type = "opencode"
command = "opencode"
args = []
full_access = true
enabled = false

[agents.grok]
type = "grok"
command = "grok"
args = []
full_access = true
enabled = false
```

Enable only CLIs installed and authenticated for the daemon's OS user. Agent
names are local aliases, so additional profiles such as `claude-plan` may use
the same `type` with different `args`. The legacy `[codex]` block remains
accepted for existing single-agent configurations.

Protect a configuration containing a plaintext token:

```sh
chmod 600 config.toml
```

Agent Relay rejects a plaintext-token config that is accessible by group or
others. To keep the token out of TOML, replace `telegram_token` with:

```toml
telegram_token_env = "AGENT_RELAY_TELEGRAM_TOKEN"
```

Then export that variable in the daemon's environment. Configuration files
using only `telegram_token_env` may be non-secret and do not require mode
`0600`. Agent Relay removes that named variable from the environment inherited
by all agent child processes.

Project aliases are relative to a configured root. Absolute paths and alias
targets that escape a root are rejected. Without an alias, a repository's
lowercase directory name is its project ID. Duplicate names receive IDs derived
from their relative paths.

The JSON state file records Telegram update offsets, selected projects and
agents, agent session IDs, pending prompts, interrupted jobs, last responses,
and undelivered Telegram messages. It therefore contains sensitive project
instructions and uses mode `0600`. Full conversation history remains in each
CLI's own data directory. Preserve both when migrating or backing up sessions.

## Build and Run

```sh
make build
./agent-relay validate --config ./config.toml
./agent-relay projects --config ./config.toml
./agent-relay run --config ./config.toml
```

You can also install the command directly:

```sh
go install github.com/sovereign313/Agent-Relay/cmd/agent-relay@latest
```

`validate` checks project discovery and verifies every enabled CLI's version,
session-resume support, structured output mode, and approval-bypass flag.

Systemd is not required. A simple detached startup is:

```sh
nohup ./agent-relay run --config ./config.toml >./var/launcher.log 2>&1 &
```

An OpenRC service example is provided at `init/agent-relay.openrc`. Adjust its
user, paths, and agent credentials before installing it.

## Telegram Commands

- `/projects` or `/list`: list discovered projects with selection buttons.
- `/project <id>`: select a project and resume its existing context when present.
- `/agents`: list enabled agents with selection buttons.
- `/agent <name>`: select an agent and resume its project context when present.
- `/sessions`: list saved project contexts for the current chat.
- `/queue`: list durable jobs for the selected project and agent.
- `/clearqueue`: discard queued and interrupted jobs for that project and agent.
- `/retry <job-id>`: explicitly retry a job interrupted by a daemon restart.
- `/new`: discard the selected project/agent session and start fresh next turn.
- `/last`: resend the last completed response for the selected project and agent.
- `/status`: show version, agent, project, state, queue, and session.
- `/cancel`: interrupt the current agent process.
- `/cancelall`: interrupt all running tasks belonging to the current chat.
- `/refresh`: rescan configured project roots.
- `/help`: show command help.

Normal text is queued for the selected project and agent. Each
chat/project/agent queue is bounded, and tasks targeting the same canonical
repository are serialized even when they come from different chats or agents.

If Agent Relay stops while an agent process is active, the job becomes
`interrupted` on restart and is not automatically replayed. This prevents a
possibly destructive request from running twice. Inspect it with `/queue`, then
use `/retry <job-id>` or `/clearqueue`.

## How Sessions Work

Agent Relay keeps session IDs separate for every chat, project, and agent. New
and resumed turns run conceptually as:

```sh
# Codex
codex exec --dangerously-bypass-approvals-and-sandbox \
  --json --output-last-message FINAL_FILE -C PROJECT_PATH -
codex exec resume --dangerously-bypass-approvals-and-sandbox \
  --json --output-last-message FINAL_FILE THREAD_ID -

# Claude Code
claude --print --output-format json \
  --dangerously-skip-permissions --session-id SESSION_ID
claude --print --output-format json \
  --dangerously-skip-permissions --resume SESSION_ID

# OpenCode
opencode run --format json --dir PROJECT_PATH \
  --dangerously-skip-permissions "PROMPT"
opencode run --format json --dir PROJECT_PATH \
  --dangerously-skip-permissions --session SESSION_ID "PROMPT"

# Grok Build
grok --no-auto-update --output-format json --cwd PROJECT_PATH \
  --always-approve --session-id SESSION_ID --single "PROMPT"
grok --no-auto-update --output-format json --cwd PROJECT_PATH \
  --always-approve --resume SESSION_ID --single "PROMPT"
```

Only the adapter's final natural-language result is returned to Telegram. Tool
events, diffs, reasoning, token usage, and command output are not forwarded.
Each adapter validates the CLI capabilities it depends on at daemon startup.
CLI output protocols can change, so upgrades may require adapter fixture
updates.

Telegram delivery retries transient transport errors, rate limits, and server
errors. A final response that still cannot be delivered remains in the outbox
for periodic retry and is also available through `/last`.

## Troubleshooting

- **No projects found:** confirm each project contains a `.git` directory or
  worktree `.git` file and is within `project_discovery_depth`.
- **Alias rejected:** aliases must resolve to a discovered Git repository below
  one configured root and may not be absolute.
- **Resume fails:** confirm the selected CLI's session data still exists and
  Agent Relay runs as the same OS user that created it.
- **Interrupted job after restart:** inspect `/queue`; retry it explicitly only
  after checking whether the previous agent invocation made partial changes.
- **Agent command not found:** set the corresponding `agents.<name>.command` to
  an absolute executable path or fix the service user's `PATH`.
- **Agent is not authenticated:** log in as the same OS user that runs Agent
  Relay, or configure the provider's API-key environment.
- **Validation reports a missing capability:** upgrade the affected CLI or
  disable its profile before starting the daemon.
- **Telegram receives no response:** inspect structured logs, verify outbound
  HTTPS access to `api.telegram.org`, and run `validate`.
- **Task will not stop:** `/cancel` sends `SIGINT` to the agent process group;
  after a grace period Go terminates a process that does not exit.

State, log, and temporary final-message files use mode `0600`. Structured logs
rotate at `log_max_bytes` and retain `log_backups` numbered files.

## Contributing

Run `make check` before opening a pull request. See
[`CONTRIBUTING.md`](CONTRIBUTING.md) for the development workflow and
[`SECURITY.md`](SECURITY.md) for private vulnerability reporting.

## License

Agent Relay is available under the [MIT License](LICENSE).
