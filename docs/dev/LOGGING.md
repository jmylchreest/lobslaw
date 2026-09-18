# Logging and secret redaction

Lobslaw uses `log/slog` through `internal/logging.New`, backed by
`github.com/jmylchreest/slog-logfilter`. The pipeline is:

```
slog -> runtime filters -> sanitizer -> JSON/text formatter -> output
```

Filters match original attributes and remain live on child loggers. Sanitization
is mandatory for the application logger, including attributes attached with
`With`, nested groups, LogValuer results, errors and JSON-shaped objects. Source,
level, timestamp and ordinary numeric fields retain their normal behavior.

The shared policy redacts credential fields, authorization/cookie values, URL
userinfo and sensitive query parameters, private-key blocks and recognizable
provider tokens. Telegram bot paths and GitHub/OpenAI/Slack token shapes are
application rules on top of the generic library redactor. No global registry
retains live credential values. Unserializable objects are replaced with a fixed
marker instead of being handed to an output formatter unsanitized.

This does not identify every secret or private fact in arbitrary prose. DEBUG
request logging still contains excerpts from all conversation messages, including
tool results. Treat those logs as private. LLM body excerpts are sanitized before
truncation so a cut cannot split a credential before its rule sees it.

## Integration rules

- Use the supplied `*slog.Logger`, `slog.Default()` or `logging.WithComponent`.
  Create production loggers only through `logging.New`.
- The CLI installs the shared logger before dispatch. Command errors and flag
  parser diagnostics go through it. Intended command data, static help and
  interactive prompts remain command output.
- HTTP error loggers and Raft standard diagnostics forward through the same
  handler. Smokescreen requires a logrus interface; its boundary adapter disables
  direct output and forwards all levels/fields to slog. It owns no filtering
  policy. Do not use logrus in application code.
- `logging.SafeError` sanitizes errors returned from Telegram APIs, including
  transport errors, download URLs and HTTP response diagnostics. It retains
  `errors.Is` and `errors.As`; trusted code can unwrap the original error, so do
  not forward the unwrapped value externally.
- `TestProductionLoggingBoundaries` guards against new independent constructors.
  New adapters must include tests proving output reaches the shared pipeline.

The library is pinned to a published immutable Go pseudo-version for the
sanitizer PR. There is no local module replacement. Merge the library PR before
Lobslaw; a subsequent tagged library release can replace the pseudo-version.
