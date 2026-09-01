package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Dream rewrites memory on its own schedule. Without this the only
// record of what it did was a log line, so "why did it merge those two
// notes" and "what has it been doing" both had no answer.

func memoryConsolidations(args []string) error {
	fs := flag.NewFlagSet("memory consolidations", flag.ExitOnError)
	var store offlineStore
	store.bind(fs)
	owner := fs.String("owner", "", "restrict to one principal (e.g. user:alice)")
	verdict := fs.String("verdict", "", "restrict to merge | keep_distinct | conflict | supersedes")
	since := fs.Duration("since", 0, "only entries newer than this (e.g. 168h)")
	limit := fs.Int("limit", 50, "maximum entries to show; 0 for all")
	full := fs.Bool("full", false, "show every source record id rather than a sample")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	s, path, err := store.open()
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	q := memory.ConsolidationQuery{
		Owner:   *owner,
		Verdict: *verdict,
		Limit:   *limit,
	}
	if *since > 0 {
		q.Since = time.Now().Add(-*since)
	}
	entries, err := memory.ListConsolidations(s, q)
	if err != nil {
		return err
	}

	return renderConsolidations(os.Stdout, entries, path, *full, *asJSON)
}

// renderConsolidations prints the log and SAYS WHERE IT CAME FROM.
//
// Shared by the live and offline forms, because "no consolidations
// recorded" is indistinguishable from the wrong store unless the
// source is on the page — and on a laptop that sentence used to be
// about a state.db the cluster never wrote.
func renderConsolidations(w io.Writer, entries []*lobslawv1.ConsolidationRecord,
	source string, full, asJSON bool) error {
	if asJSON {
		out := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			out = append(out, consolidationJSON(e))
		}
		return emitJSON(map[string]any{"source": source, "consolidations": out})
	}

	_, _ = fmt.Fprintf(w, "%s\n", source)
	if len(entries) == 0 {
		_, _ = fmt.Fprintln(w, "no consolidations recorded.")
		return nil
	}
	for _, e := range entries {
		when := ""
		if e.CreatedAt != nil {
			when = e.CreatedAt.AsTime().Format("2006-01-02 15:04")
		}
		status := ""
		if !e.Applied {
			// Loudest thing on the line: a decision that was made and
			// then failed to apply is the case a user is looking for.
			status = "  !! NOT APPLIED: " + e.Error
		}
		_, _ = fmt.Fprintf(w, "%s  %-14s %d records (avg similarity %.2f)%s\n",
			when, e.Verdict, e.MemberCount, e.AvgSimilarity, status)
		if e.Reason != "" {
			_, _ = fmt.Fprintf(w, "    reason:  %s\n", e.Reason)
		}
		if e.ResultId != "" {
			_, _ = fmt.Fprintf(w, "    result:  %s\n", e.ResultId)
		}
		_, _ = fmt.Fprintf(w, "    sources: %s\n", renderSources(e.SourceIds, full))
	}
	_, _ = fmt.Fprintf(w, "\n%d entr%s.\n", len(entries), plural(len(entries)))
	return nil
}

// renderSources keeps a default listing readable. A cluster of twenty
// near-duplicates is common and printing every id buries the reason,
// which is the part somebody came here to read.
func renderSources(ids []string, full bool) string {
	const sample = 4
	if full || len(ids) <= sample {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s … and %d more (--full to see all)",
		strings.Join(ids[:sample], ", "), len(ids)-sample)
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func consolidationJSON(e *lobslawv1.ConsolidationRecord) map[string]any {
	m := map[string]any{
		"id":             e.Id,
		"cluster_id":     e.ClusterId,
		"verdict":        e.Verdict,
		"reason":         e.Reason,
		"source_ids":     e.SourceIds,
		"result_id":      e.ResultId,
		"member_count":   e.MemberCount,
		"avg_similarity": e.AvgSimilarity,
		"owner":          e.Owner,
		"applied":        e.Applied,
	}
	if e.Error != "" {
		m["error"] = e.Error
	}
	if e.CreatedAt != nil {
		m["created_at"] = e.CreatedAt.AsTime()
	}
	return m
}
