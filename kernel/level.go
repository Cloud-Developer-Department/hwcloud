package kernel

import "log/slog"

// LevelTrace is the trace log level. slog has no built-in trace; -8 is
// the convention from the standard library's custom-levels example.
// Prompt dumps (kernel/run.go) log at this level and stay filtered
// unless the configured log level is "trace" — prompt content may
// contain user data and secrets, hence the explicit opt-in.
const LevelTrace = slog.Level(-8)
