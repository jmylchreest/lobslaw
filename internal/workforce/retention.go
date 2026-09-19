package workforce

import "sort"

// Chat display and completed chat execution rows are a rolling view. Full
// transcripts remain in the session store. Dedupe targets and live dependencies
// are never discarded; retention cannot make a previous delivery executable.
func retainChat(st *State, incomingDependencies ...string) {
	protected := map[string]bool{}
	for _, id := range incomingDependencies {
		protected[id] = true
	}
	for _, id := range st.Events {
		protected[id] = true
	}
	for _, t := range st.Tasks {
		if t.Status == StatusDone || t.Status == StatusFailed || t.Status == StatusCancelled {
			continue
		}
		for _, id := range t.DependsOn {
			protected[id] = true
		}
		if t.ParentID != "" {
			protected[t.ParentID] = true
		}
	}
	eligible := []*Task{}
	for id, t := range st.Tasks {
		x := st.Executions[id]
		if x != nil && x.Chat && x.TranscriptSaved && len(x.ArtifactReferences) == 0 && t.Status == StatusDone && !protected[id] {
			eligible = append(eligible, t)
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].UpdatedAt.Equal(eligible[j].UpdatedAt) {
			return eligible[i].ID > eligible[j].ID
		}
		return eligible[i].UpdatedAt.After(eligible[j].UpdatedAt)
	})
	for _, t := range eligible[min(len(eligible), MaxRetainedChats):] {
		delete(st.Tasks, t.ID)
		delete(st.Executions, t.ID)
	}
	messages := make([]ProjectMessage, 0, len(st.Messages))
	for i, m := range st.Messages {
		t := st.Tasks[m.TaskID]
		active := t != nil && t.Status != StatusDone && t.Status != StatusFailed && t.Status != StatusCancelled
		if i >= len(st.Messages)-MaxChatMessages || active {
			messages = append(messages, m)
		}
	}
	st.Messages = messages
}
