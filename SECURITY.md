# Security Policy

Agent Relay is intentionally capable of remotely instructing a local coding
agent. A vulnerability can expose source code, credentials, filesystem data, or
command execution on the host.

## Reporting

Report suspected vulnerabilities through
[GitHub private security advisories](https://github.com/sovereign313/Agent-Relay/security/advisories/new).
Do not include secrets in a public issue.

Include the affected commit or version, impact, reproduction steps, and any
suggested mitigation. Sanitize Telegram tokens, agent credentials, prompts,
logs, paths, and private repository content.

## Deployment Expectations

- Use a dedicated Telegram bot and a strict numeric user allowlist.
- Keep `private_chats_only = true`.
- Prefer `telegram_token_env` over storing the bot token in TOML.
- Run under a dedicated OS user with access only to intended project roots.
- Treat `full_access = true` as unrestricted command execution by that OS user.
- Keep configuration, state, logs, and agent session data private.
- Review interrupted work before explicitly retrying it.

Project-root selection is an input restriction, not a security sandbox. Host or
container isolation is required when coding agents must not reach other data.
