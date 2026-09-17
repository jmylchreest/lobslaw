package main

import (
	"bufio"
	"bytes"
	"flag"
	"slices"
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

func TestArchiveConflictPromptOffersOriginalForMappedGroups(t *testing.T) {
	for _, kind := range []string{"sessions", "scheduled-tasks", "documents"} {
		t.Run(kind, func(t *testing.T) {
			ref := memory.ArchiveRecordRef{Kind: kind, ID: "record"}
			plan := memory.ArchiveImportPlan{
				Conflicts:       []memory.ArchiveRecordRef{ref},
				ConflictDetails: []memory.ArchiveConflict{{Kind: kind, ID: ref.ID, DestinationID: "mapped", Reason: "source or import options changed"}},
			}
			var opts memory.ArchiveImportOptions
			var out bytes.Buffer
			if err := promptArchiveConflicts(bufio.NewReader(strings.NewReader("replace-original\nskip\n")), &out, plan, &opts); err != nil {
				t.Fatal(err)
			}
			if kind == "documents" {
				if len(opts.ReplaceOriginal) != 0 || !slices.Contains(opts.Skip, ref) || strings.Contains(out.String(), "replace-original") {
					t.Fatalf("unsupported choice: %+v %s", opts, &out)
				}
			} else if !slices.Contains(opts.ReplaceOriginal, ref) || !strings.Contains(out.String(), "retires the mapped copy") {
				t.Fatalf("missing original choice: %+v %s", opts, &out)
			}
		})
	}
}

func TestArchiveConflictPromptSupersedesOriginalFlag(t *testing.T) {
	for _, answer := range []string{"replace", "skip"} {
		t.Run(answer, func(t *testing.T) {
			var opts memory.ArchiveImportOptions
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			bindArchiveImportOptions(fs, &opts)
			if err := fs.Parse([]string{"--replace-original", "sessions/chat", "--replace-original", "scheduled-tasks/other"}); err != nil {
				t.Fatal(err)
			}
			plan := memory.ArchiveImportPlan{
				Conflicts:       []memory.ArchiveRecordRef{{Kind: "sessions", ID: "chat"}},
				ConflictDetails: []memory.ArchiveConflict{{Kind: "sessions", ID: "chat", DestinationID: "mapped", Reason: "alongside sessions: destination changed"}},
			}
			var out bytes.Buffer
			// A blocked original choice cannot be selected again indefinitely.
			if err := promptArchiveConflicts(bufio.NewReader(strings.NewReader("replace-original\n"+answer+"\n")), &out, plan, &opts); err != nil {
				t.Fatal(err)
			}
			if len(opts.ReplaceOriginal) != 1 || opts.ReplaceOriginal[0].ID != "other" || strings.Contains(out.String(), "replace-original/") {
				t.Fatalf("stale selection: %+v %s", opts, &out)
			}
			selected := opts.Replace
			if answer == "skip" {
				selected = opts.Skip
			}
			if len(selected) != 1 || selected[0].ID != "chat" {
				t.Fatalf("new choice lost: %+v", opts)
			}
		})
	}
}
