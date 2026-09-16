package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/memory"
)

func TestArchiveConflictPromptGroupsSessions(t *testing.T) {
	plan := memory.ArchiveImportPlan{Conflicts: []memory.ArchiveRecordRef{
		{Kind: "sessions", ID: "chat"}, {Kind: "session-messages", ID: "chat:00000000000000000001"},
	}}
	var opts memory.ArchiveImportOptions
	var out bytes.Buffer
	if err := promptArchiveConflicts(bufio.NewReader(strings.NewReader("alongside\n")), &out, plan, &opts); err != nil {
		t.Fatal(err)
	}
	if len(opts.Alongside) != 1 || opts.Alongside[0].Kind != "sessions" {
		t.Fatalf("options: %+v", opts)
	}
}

func TestArchiveConflictPromptCancelsOnEOF(t *testing.T) {
	plan := memory.ArchiveImportPlan{Conflicts: []memory.ArchiveRecordRef{{Kind: "sessions", ID: "chat"}}}
	var opts memory.ArchiveImportOptions
	if err := promptArchiveConflicts(bufio.NewReader(strings.NewReader("")), &bytes.Buffer{}, plan, &opts); err == nil {
		t.Fatal("EOF must cancel")
	}
}

func TestArchiveConflictPromptDoesNotDuplicateMappedTranscript(t *testing.T) {
	plan := memory.ArchiveImportPlan{
		Conflicts:       []memory.ArchiveRecordRef{{Kind: "session-messages", ID: "chat:00000000000000000001"}},
		ConflictDetails: []memory.ArchiveConflict{{Kind: "session-messages", ID: "chat:00000000000000000001", DestinationID: "mapped:00000000000000000001", Reason: "destination changed"}},
	}
	var opts memory.ArchiveImportOptions
	if err := promptArchiveConflicts(bufio.NewReader(strings.NewReader("alongside\nskip\n")), &bytes.Buffer{}, plan, &opts); err != nil {
		t.Fatal(err)
	}
	if len(opts.Alongside) != 0 || len(opts.Skip) != 1 || opts.Skip[0].ID != "chat" {
		t.Fatalf("options: %+v", opts)
	}
}
