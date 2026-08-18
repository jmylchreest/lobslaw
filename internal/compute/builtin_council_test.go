package compute

import (
	"strings"
	"testing"
)

// A real deployment was asked "what skills do you have?" and answered
// with a table of PROVIDERS, headed "Capabilities": chat, vision,
// image, speak. Those are modalities.
//
// The model was not being careless. list_providers was the only
// inventory-shaped tool in the set, and its description invited the
// call — it said to use it when the user asks "which model does what",
// which is close enough to "what can you do" that the routing is
// reasonable. The word "capabilities" in the output then collided with
// the everyday meaning of "skills".
//
// So the description has to carry the disambiguation, because nothing
// else in the turn will. This test fails if somebody removes it.
func TestListProvidersDescriptionDoesNotInviteSkillQuestions(t *testing.T) {
	var desc string
	for _, def := range CouncilToolDefs() {
		if def.Name == "list_providers" {
			desc = def.Description
		}
	}
	if desc == "" {
		t.Fatal("list_providers has no ToolDef; the description this test guards is gone")
	}

	// The phrase that attracted the wrong call.
	if strings.Contains(desc, "which model does what") {
		t.Error("description still says \"which model does what\" — that is what pulled " +
			"\"what skills do you have\" onto this tool")
	}

	// The disambiguation must be explicit. A description that merely
	// omits the invitation is not the same as one that redirects: the
	// model still has to be told where the real answer lives.
	for _, want := range []string{
		"NOT the skills list",
		"Installed Skills",
		"do NOT call this tool",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("description lost its disambiguation: missing %q", want)
		}
	}
}

// The negative half. "capabilities" is the column heading the wrong
// answer used, so the description must say what it means here rather
// than leaving the model to infer it.
func TestListProvidersDescriptionDefinesCapabilities(t *testing.T) {
	var desc string
	for _, def := range CouncilToolDefs() {
		if def.Name == "list_providers" {
			desc = def.Description
		}
	}
	if !strings.Contains(desc, "modalities") {
		t.Error("description does not say that \"capabilities\" means modalities; " +
			"that ambiguity is the bug this guards")
	}
}
