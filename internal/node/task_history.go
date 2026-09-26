package node

import (
	"sort"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

const coordinatorHistoryMessages = 100

func (n *Node) coordinatorTaskHistory(owner, bot string) ([]compute.Message, error) {
	var tasks []*pb.TaskApprovalRecord
	err := n.store.ForEach(memory.BucketTaskApprovals, func(_ string, raw []byte) error {
		task := new(pb.TaskApprovalRecord)
		if err := proto.Unmarshal(raw, task); err != nil {
			return err
		}
		if task.Owner == owner && task.Actor == "bot:"+bot && task.CoordinatorConversation && len(task.Transcript) > 0 {
			tasks = append(tasks, task)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Id < tasks[j].Id })
	start, count := len(tasks), 0
	for start > 0 && count < coordinatorHistoryMessages {
		start--
		count += len(tasks[start].Transcript)
	}
	var messages []compute.Message
	for _, task := range tasks[start:] {
		for _, message := range task.Transcript {
			messages = append(messages, turn.MessageFromProto(message))
		}
	}
	return messages, nil
}
