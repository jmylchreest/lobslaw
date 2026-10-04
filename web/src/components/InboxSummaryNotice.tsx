import type { InboxItem } from "../api";

export function InboxSummaryNotice({ item }: { item: InboxItem }) {
  if (!item.truncated_fields?.length) return null;
  // Derive the owner-authorized route from identity, never follow arbitrary
  // URLs supplied in a record's display text or omitted link fields.
  const detail = `/v1/inbox/${encodeURIComponent(item.recipient)}/${encodeURIComponent(item.id)}`;
  return (
    <div className="meta" role="note">
      Shortened activity summary ({item.truncated_fields.join(", ")}).{" "}
      <a href={detail}>View full record</a>
    </div>
  );
}
