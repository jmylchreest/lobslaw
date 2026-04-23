package promptgen

import (
	"fmt"
	"strings"
)

// TrustLevel classifies how much we trust the content we're about
// to show the model. The delimiter shape tells the model whether to
// treat the content as authoritative instructions or as untrusted
// data (memory recall, tool output, fetched web pages, message
// content from untrusted users).
//
// Per the safety principles in BuildSafety: content inside
// <untrusted> delimiters is DATA, not orders. The model is trained
// (via the safety block) to refuse embedded instructions within
// untrusted regions.
type TrustLevel int

const (
	// TrustTrusted — operator-authored content (e.g. the assembled
	// system prompt itself). No delimiters needed; included here
	// only so callers can pass a single enum through helpers.
	TrustTrusted TrustLevel = iota

	// TrustUntrusted — anything the caller doesn't vouch for.
	// Default for tool output, memory recall, fetched content,
	// skill output. Rendered inside <untrusted> ... </untrusted>.
	TrustUntrusted

	// TrustUntrustedUser — a specialised subset of TrustUntrusted
	// for messages that came from a human over a channel. Same
	// delimiter shape + an attribution attr so the model can
	// distinguish "user said this" from "a tool returned this text".
	TrustUntrustedUser
)

// ContextBlock is a labelled chunk of data the agent wants to
// expose to the model. Source labels the origin (e.g. "memory:recall",
// "tool:bash:stdout", "channel:telegram"); Content is the raw bytes
// (verbatim into the prompt after delimiter wrapping).
type ContextBlock struct {
	Source  string
	Trust   TrustLevel
	Content string
}

// WrapContext renders one or more ContextBlocks into a single
// string block with delimiter wrapping per trust level. Multiple
// blocks are emitted in input order — callers have already decided
// priority (e.g. memory recall before tool output).
//
// Empty blocks are elided. Empty total output yields "" (not an
// empty <untrusted></untrusted> tag pair — those would just confuse
// a reader).
//
// Delimiter choice: explicit XML-like tags (<untrusted>, </untrusted>)
// because LLMs handle them reliably at tokenization boundaries and
// they're distinctive enough to escape from nested content via
// source attribute. We do NOT attempt to escape < > in user content
// — a user who includes `</untrusted>` in their message CAN close the
// block; the safety training on the model side treats this as
// attempted injection and surfaces it to the user rather than
// obeying.
func WrapContext(blocks []ContextBlock) string {
	var b strings.Builder
	for _, block := range blocks {
		if block.Content == "" {
			continue
		}
		switch block.Trust {
		case TrustTrusted:
			fmt.Fprintf(&b, "<!-- source:%s -->\n%s\n", block.Source, block.Content)
		case TrustUntrusted:
			fmt.Fprintf(&b, "<untrusted source=%q>\n%s\n</untrusted>\n", block.Source, block.Content)
		case TrustUntrustedUser:
			fmt.Fprintf(&b, "<untrusted-user source=%q>\n%s\n</untrusted-user>\n", block.Source, block.Content)
		default:
			// Unknown trust levels → fall through to untrusted.
			// Never up-trust an unknown level to trusted.
			fmt.Fprintf(&b, "<untrusted source=%q>\n%s\n</untrusted>\n", block.Source, block.Content)
		}
	}
	return b.String()
}
