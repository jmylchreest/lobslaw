package memory

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	bolt "go.etcd.io/bbolt"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

const contractKey = "cluster_contract"

var ErrUpgradeConflict = errors.New("upgrade transition conflict")

func readContract(tx *bolt.Tx) (dataformat.ContractState, error) {
	state := dataformat.ContractState{Active: 1}
	if b := tx.Bucket([]byte(formatBucket)); b != nil {
		if raw := b.Get([]byte(contractKey)); raw != nil {
			if err := json.Unmarshal(raw, &state); err != nil {
				return state, err
			}
		} else {
			format, err := readStateFormat(tx)
			if err != nil {
				return state, err
			}
			if format.Version > 0 {
				state.Active = uint32(format.Version)
			}
		}
	}
	return state, state.Validate(dataformat.SupportedContracts())
}

func (s *Store) ContractState() (dataformat.ContractState, error) {
	var state dataformat.ContractState
	err := s.loadDB().View(func(tx *bolt.Tx) error { var err error; state, err = readContract(tx); return err })
	return state, err
}

func commandTransition(c *pb.UpgradeCommand, index uint64) dataformat.Transition {
	return dataformat.Transition{ID: c.TransitionId, Target: c.Target, Epoch: c.ExpectedEpoch, MembershipIndex: c.MembershipIndex, MembershipFingerprint: c.MembershipFingerprint, Members: c.MemberIds, Index: index}
}

func (s *Store) applyUpgrade(c *pb.UpgradeCommand, index uint64) error {
	return s.loadDB().Update(func(tx *bolt.Tx) error {
		state, err := readContract(tx)
		if err != nil {
			return err
		}
		next, err := state.Advance(c.Action, commandTransition(c, index), dataformat.SupportedContracts())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUpgradeConflict, err)
		}
		if next.Active != state.Active {
			if err := activateContract(tx, next.Active); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return err
		}
		b, err := tx.CreateBucketIfNotExists([]byte(formatBucket))
		if err != nil {
			return err
		}
		if err := b.Put([]byte(contractKey), raw); err != nil {
			return err
		}
		// Publish the activation and its replay watermark in the same transaction.
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], index)
		sealed, err := s.cipher.Seal(buf[:])
		if err != nil {
			return err
		}
		meta, err := tx.CreateBucketIfNotExists([]byte(BucketRaftMeta))
		if err != nil {
			return err
		}
		return meta.Put([]byte(KeyLastAppliedIndex), sealed)
	})
}

// Feature branches extend this transaction with their actual schema migration.
func activateContract(_ *bolt.Tx, target uint32) error {
	if target != 1 {
		return fmt.Errorf("unsupported contract %d", target)
	}
	return nil
}

// validateContractEntry runs before proposal as well as on every replica. Feature
// branches add explicit new-payload gates here, including nested archive batches.
func validateContractEntry(entry *pb.LogEntry, _ dataformat.ContractState) error {
	return validateLogEntrySupport(entry)
}
