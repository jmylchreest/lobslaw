package node

import (
	"bytes"
	"context"
	"errors"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func (n *Node) checkSharedTask(_ context.Context, task *lobslawv1.ScheduledTaskRecord) error {
	if n.store == nil || n.skillRegistry == nil {
		return errors.New("sharing: approval store and skill registry required")
	}
	a, err := memory.NewSharingStore(n.raft, n.store).CheckTask(task)
	if err != nil {
		return err
	}
	validator := &skillService{policy: n.skillSigningPolicy, verifier: n.skillVerifier}
	if err := validator.validateShare(a); err != nil {
		return err
	}
	p := a.Package()
	winner, err := n.skillRegistry.Get(p.Name)
	if err != nil {
		return err
	}
	bundle, err := memory.ReadBundle(winner.ManifestDir)
	if err != nil {
		return err
	}
	if !bytes.Equal(bundle.Manifest, p.Manifest) || !bytes.Equal(bundle.Signature, p.ManifestSignature) || len(bundle.Files) != len(p.Files) {
		return errors.New("sharing: loaded skill differs from approved release; wait for reconciliation or resolve override")
	}
	for path, raw := range p.Files {
		if !bytes.Equal(raw, bundle.Files[path]) {
			return errors.New("sharing: loaded skill file differs from approved release")
		}
	}
	return nil
}
