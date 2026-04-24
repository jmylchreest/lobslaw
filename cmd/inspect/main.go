// Package main is a one-off inspector for the encrypted lobslaw
// state.db. Dumps episodic + vector records in JSON for operator
// debugging. Not part of the shipped binary — run via
// `go run ./cmd/inspect <path-to-state.db>`. LOBSLAW_MEMORY_KEY
// must be exported.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: inspect <path-to-state.db>")
		os.Exit(1)
	}
	path := os.Args[1]
	keyRaw := os.Getenv("LOBSLAW_MEMORY_KEY")
	if keyRaw == "" {
		fmt.Fprintln(os.Stderr, "LOBSLAW_MEMORY_KEY env var required")
		os.Exit(1)
	}
	keyBytes, err := base64.StdEncoding.DecodeString(keyRaw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "decode key:", err)
		os.Exit(1)
	}
	var key crypto.Key
	copy(key[:], keyBytes)

	store, err := memory.OpenStore(path, key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer store.Close()

	fmt.Println("=== EPISODIC RECORDS ===")
	count := 0
	_ = store.ForEach(memory.BucketEpisodicRecords, func(_ string, v []byte) error {
		var r lobslawv1.EpisodicRecord
		if err := proto.Unmarshal(v, &r); err != nil {
			return nil
		}
		count++
		j, _ := json.MarshalIndent(map[string]any{
			"id":         r.Id,
			"event":      r.Event,
			"context":    r.Context,
			"importance": r.Importance,
			"tags":       r.Tags,
			"retention":  r.Retention,
			"timestamp":  r.Timestamp.AsTime().Format("2006-01-02 15:04:05 UTC"),
		}, "", "  ")
		fmt.Println(string(j))
		fmt.Println("---")
		return nil
	})
	fmt.Printf("\nTotal episodic records: %d\n\n", count)

	fmt.Println("=== VECTOR RECORDS ===")
	vcount := 0
	_ = store.ForEach(memory.BucketVectorRecords, func(_ string, v []byte) error {
		var r lobslawv1.VectorRecord
		if err := proto.Unmarshal(v, &r); err != nil {
			return nil
		}
		vcount++
		fmt.Printf("  id=%s scope=%s dims=%d source_ids=%v text_len=%d\n",
			r.Id, r.Scope, len(r.Embedding), r.SourceIds, len(r.Text))
		return nil
	})
	fmt.Printf("\nTotal vector records: %d\n", vcount)
}
