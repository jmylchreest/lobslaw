// Package promptgen assembles system prompts for agent runs. It wraps
// untrusted content (tool output, memory recall, skill output) in trust
// delimiters so the model can defend against prompt injection, and
// truncates bootstrap files to budget limits.
package promptgen
