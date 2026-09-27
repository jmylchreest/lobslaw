package node

import (
	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/turn"
)

func (n *Node) coordinatorTaskHistory(owner, bot string) ([]compute.Message, error) {
	tasks, err := n.store.CoordinatorTaskHistory(owner, "bot:"+bot)
	if err != nil {
		return nil, err
	}
	var messages []compute.Message
	for _, task := range tasks {
		for _, message := range task.Transcript {
			messages = append(messages, turn.MessageFromProto(message))
		}
	}
	return messages, nil
}
