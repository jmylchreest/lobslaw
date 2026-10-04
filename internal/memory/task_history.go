package memory

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"slices"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

const (
	// The index contains only empty markers; evidence remains encrypted in
	// task_approvals. Hex components make arbitrary principal IDs unambiguous.
	bucketTaskHistory        = "task_history_v1"
	TaskHistoryMessageTarget = 100
	TaskHistoryMaxTasks      = 100
	TaskHistoryMaxBytes      = 8 << 20
	TaskHistoryMaxMessages   = 65536
)

func taskHistoryPrefix(owner, actor string) []byte {
	return []byte(hex.EncodeToString([]byte(owner)) + "/" + hex.EncodeToString([]byte(actor)) + "/")
}

func taskHistoryKey(id string, task *pb.TaskApprovalRecord) []byte {
	if task.Owner == "" || task.Actor == "" || !task.CoordinatorConversation || len(task.Transcript) == 0 {
		return nil
	}
	return append(taskHistoryPrefix(task.Owner, task.Actor), id...)
}

// This runs inside the record's transaction, including admission and FSM CAS
// writes. No second proposal or asynchronous projection can lag a resumed turn.
func (s *Store) updateTaskHistory(tx *bolt.Tx, id string, raw []byte) error {
	index := tx.Bucket([]byte(bucketTaskHistory))
	if index == nil {
		state, err := readContract(tx)
		if err != nil {
			return err
		}
		if state.Active < 2 {
			return nil
		}
		return fmt.Errorf("missing task history index")
	}
	if sealed := tx.Bucket([]byte(BucketTaskApprovals)).Get([]byte(id)); sealed != nil {
		previous, err := s.cipher.OpenTo(nil, sealed)
		if err != nil {
			return fmt.Errorf("decrypt previous task history: %w", err)
		}
		var task pb.TaskApprovalRecord
		if err := proto.Unmarshal(previous, &task); err != nil {
			return fmt.Errorf("decode previous task history: %w", err)
		}
		if key := taskHistoryKey(id, &task); key != nil {
			if err := index.Delete(key); err != nil {
				return err
			}
		}
	}
	if raw == nil {
		return nil
	}
	var task pb.TaskApprovalRecord
	if err := proto.Unmarshal(raw, &task); err != nil {
		return fmt.Errorf("decode task history: %w", err)
	}
	if key := taskHistoryKey(id, &task); key != nil {
		return index.Put(key, nil)
	}
	return nil
}

// Rebuild even an existing index: an older binary may have written the same
// database or snapshot without maintaining it. Do this before publication,
// never as a fallback on a coordinator turn. Only derived markers change.
func (s *Store) rebuildTaskHistory(tx *bolt.Tx) error {
	if tx.Bucket([]byte(bucketTaskHistory)) != nil {
		if err := tx.DeleteBucket([]byte(bucketTaskHistory)); err != nil {
			return err
		}
	}
	index, err := tx.CreateBucket([]byte(bucketTaskHistory))
	if err != nil {
		return err
	}
	var buf []byte
	return tx.Bucket([]byte(BucketTaskApprovals)).ForEach(func(id, sealed []byte) error {
		raw, err := s.cipher.OpenTo(buf, sealed)
		if err != nil {
			return fmt.Errorf("decrypt task %s: %w", id, err)
		}
		buf = raw
		var task pb.TaskApprovalRecord
		if err := proto.Unmarshal(raw, &task); err != nil {
			return fmt.Errorf("decode task %s: %w", id, err)
		}
		if key := taskHistoryKey(string(id), &task); key != nil {
			return index.Put(key, nil)
		}
		return nil
	})
}

// CoordinatorTaskHistory returns whole task transcripts, ordered by task ID
// as before. The message target is soft so tool-call/result batches never split.
// Ciphertext lengths are checked before decryption, bounding read/decode work
// even for legacy records larger than today's checkpoint limit.
func (s *Store) CoordinatorTaskHistory(owner, actor string) ([]*pb.TaskApprovalRecord, error) {
	var tasks []*pb.TaskApprovalRecord
	if owner == "" || actor == "" {
		return tasks, nil
	}
	err := s.loadDB().View(func(tx *bolt.Tx) error {
		index := tx.Bucket([]byte(bucketTaskHistory))
		if index == nil {
			return fmt.Errorf("task history index unavailable; reopen writable store to rebuild")
		}
		prefix := taskHistoryPrefix(owner, actor)
		// Prefix ends in '/', so its exclusive upper bound is always defined.
		upper := bytes.Clone(prefix)
		upper[len(upper)-1]++
		cursor := index.Cursor()
		key, _ := cursor.Seek(upper)
		if key == nil {
			key, _ = cursor.Last()
		} else {
			key, _ = cursor.Prev()
		}
		count, size := 0, 0
		for ; key != nil && bytes.HasPrefix(key, prefix); key, _ = cursor.Prev() {
			if len(tasks) >= TaskHistoryMaxTasks || count >= TaskHistoryMessageTarget {
				break
			}
			id := key[len(prefix):]
			sealed := tx.Bucket([]byte(BucketTaskApprovals)).Get(id)
			if sealed == nil {
				return fmt.Errorf("task history index references missing task %s", id)
			}
			if len(sealed) > TaskHistoryMaxBytes-size {
				if len(tasks) == 0 {
					return fmt.Errorf("latest task history exceeds byte limit")
				}
				break
			}
			size += len(sealed)
			raw, err := s.cipher.OpenTo(nil, sealed)
			if err != nil {
				return fmt.Errorf("decrypt task history: %w", err)
			}
			task := new(pb.TaskApprovalRecord)
			if err := proto.Unmarshal(raw, task); err != nil {
				return fmt.Errorf("decode task history: %w", err)
			}
			if !bytes.Equal(taskHistoryKey(string(id), task), key) {
				return fmt.Errorf("task history index mismatch")
			}
			if len(task.Transcript) > TaskHistoryMaxMessages-count {
				if len(tasks) == 0 {
					return fmt.Errorf("latest task history exceeds message limit")
				}
				break
			}
			count += len(task.Transcript)
			tasks = append(tasks, task)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Reverse(tasks)
	return tasks, nil
}
