// Package notify is the channel-agnostic notification dispatch
// layer. The agent calls a single builtin (`notify`) with a
// canonical user_id + body; the service walks the user's
// preferences (BucketUserPrefs), resolves channel addresses, and
// dispatches to every registered Sink for those channel types.
//
// Two delivery modes:
//
//  1. Originator-channel reply: when Notification.OriginatorChannel
//     is set, deliver only on that channel (the inbound message's
//     channel) using the address from the user's prefs. Mirrors how
//     a real conversation works — reply where the question came from.
//
//  2. Broadcast: when OriginatorChannel is empty (commitments,
//     scheduled tasks, async research completions), deliver to
//     every channel address bound to that user_id. No per-event
//     channel selection — operator-set preferences win.
//
// Notifications carry an ExpiresAt. Sinks check it at delivery
// time and drop expired notifications with an audit-log line.
// Default TTL is 5 minutes for low-urgency events; high-urgency
// events get longer (configurable per-call). The expiry guards
// against stale "remind me at 8am" messages getting delivered at
// 11pm after a multi-hour cluster outage.
package notify
