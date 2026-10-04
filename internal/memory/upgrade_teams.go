package memory

import (
	"slices"

	"google.golang.org/protobuf/reflect/protoreflect"
)

var teamBuckets = []string{BucketBots, BucketGroups, BucketBotInbox, bucketTaskHistory, bucketInboxActivity, bucketInboxNotifications, bucketInboxNotificationKeys}

func isTeamBucket(name string) bool { return slices.Contains(teamBuckets, name) }

// These fields change execution semantics, not just presentation. Older
// runners must never receive a continuation with restrictions they ignore.
func usesContract2Fields(message protoreflect.Message) bool {
	found := false
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		name := string(message.Descriptor().Name())
		number := field.Number()
		if (name == "Continuation" && number >= 14 && number <= 17) || (name == "SessionMessage" && number == 10) || (name == "TaskApprovalRecord" && number >= 101 && number <= 106) {
			found = true
			return false
		}
		switch {
		case field.IsMap():
			if field.MapValue().Kind() == protoreflect.MessageKind {
				value.Map().Range(func(_ protoreflect.MapKey, v protoreflect.Value) bool {
					found = usesContract2Fields(v.Message())
					return !found
				})
			}
		case field.IsList():
			if field.Kind() == protoreflect.MessageKind {
				list := value.List()
				for i := 0; i < list.Len() && !found; i++ {
					found = usesContract2Fields(list.Get(i).Message())
				}
			}
		case field.Kind() == protoreflect.MessageKind:
			found = usesContract2Fields(value.Message())
		}
		return !found
	})
	return found
}
