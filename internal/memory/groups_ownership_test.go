package memory

import (
	"testing"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestEmptyGroupOwnerIsInaccessible(t *testing.T) {
	t.Parallel()
	unowned := &lobslawv1.GroupRecord{Id: "orphan", Name: "Orphan", Owner: ""}
	if MayModifyGroup(unowned, "user:alice") {
		t.Fatal("empty owner must not be public")
	}
	if MayModifyGroup(unowned, "") {
		t.Fatal("empty principal must not grant")
	}
	owned := &lobslawv1.GroupRecord{Id: "core", Name: "Core", Owner: "user:alice"}
	if !MayModifyGroup(owned, "user:alice") {
		t.Fatal("owner must be able to modify")
	}
	if MayModifyGroup(owned, "user:sam") {
		t.Fatal("another person must not modify")
	}
}

func TestGroupsAndInboxAreExportable(t *testing.T) {
	t.Parallel()
	want := map[string]bool{BucketGroups: false, BucketBotInbox: false}
	for _, k := range archiveKinds {
		if _, ok := want[k.bucket]; ok {
			want[k.bucket] = true
		}
	}
	for bucket, found := range want {
		if !found {
			t.Errorf("%q is not in archiveKinds", bucket)
		}
	}
}

func TestGroupOfEmptyIsNotASharedDefault(t *testing.T) {
	t.Parallel()
	if got := GroupOf(&lobslawv1.BotRecord{Id: "x"}); got != "" {
		t.Fatalf("empty group_id = %q, want empty (never another person's team)", got)
	}
}
