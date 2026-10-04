package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func clusterUpgrade(args []string) error {
	if len(args) == 0 {
		return errors.New("cluster upgrade requires status, prepare, finalize, abort or transfer")
	}
	action := args[0]
	if action != "status" && action != "prepare" && action != "finalize" && action != "abort" && action != "transfer" {
		return errors.New("unknown upgrade action")
	}
	fs := newFlagSet("cluster upgrade "+action, flag.ContinueOnError)
	var node liveNode
	node.bind(fs)
	member := fs.String("member", "", "target voter for leadership transfer")
	id := fs.String("id", "", "unique transition id; reuse on retry")
	target := fs.Uint("target", 0, "target data contract")
	epoch := fs.Uint64("epoch", 0, "expected epoch from upgrade status")
	positionals, err := parseFlagsAndPositionals(fs, args[1:])
	if err != nil {
		return err
	}
	if len(positionals) != 0 {
		return errors.New("unexpected upgrade arguments")
	}
	if action != "status" && action != "transfer" && (*id == "" || *target == 0 || uint64(*target) > uint64(^uint32(0))) {
		return errors.New("--id and valid --target are required")
	}
	if action == "transfer" && *member == "" {
		return errors.New("--member is required")
	}
	conn, err := node.dial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := node.ctx()
	defer cancel()
	client := pb.NewUpgradeServiceClient(conn)
	var out *pb.UpgradeStatusResponse
	if action == "status" {
		out, err = client.UpgradeStatus(ctx, &pb.UpgradeStatusRequest{})
	} else {
		var reply *pb.ChangeUpgradeResponse
		reply, err = client.ChangeUpgrade(ctx, &pb.ChangeUpgradeRequest{Action: action, TransitionId: *id, Target: uint32(*target), ExpectedEpoch: *epoch, TargetNodeId: *member})
		out = reply.GetStatus()
	}
	if err != nil {
		return err
	}
	raw, err := (protojson.MarshalOptions{Indent: "  ", UseProtoNames: true, EmitUnpopulated: true}).Marshal(out)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, string(raw))
	return err
}
