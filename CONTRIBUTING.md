# Contributing

Agent Relay exposes a coding agent through Telegram, so changes should preserve
its authorization boundaries, durable job semantics, and redaction guarantees.

## Development

Requirements:

- Go 1.23 or newer
- Linux for process-group behavior
- One or more supported agent CLIs for manual end-to-end testing

Build and validate the repository:

```sh
make check
make build
```

`make check` runs unit tests, `go vet`, and the race detector. Add focused tests
for behavior changes, particularly around persistence, queue transitions,
authorization, process cancellation, and Telegram delivery retries.

## Pull Requests

- Keep changes focused and explain the user-visible behavior.
- Do not commit bot tokens, agent credentials, state files, logs, prompts, or
  private repository content.
- Document new configuration keys and Telegram commands in the README and
  example configuration.
- Note any manual agent or Telegram validation performed.
- Call out changes that broaden filesystem, process, or network access.

Use GitHub's private security advisory flow for vulnerabilities rather than a
public issue.
