import { describe, expect, it } from "vitest";
import { fromBinary, fromJson, toBinary, toJson } from "@bufbuild/protobuf";
import { ConsoleBotPatchSchema, ConsoleBotSchema, TaskApprovalRecordSchema } from "./gen/lobslaw/v1/lobslaw_pb";

describe("generated console contracts", () => {
 it("preserves omitted, false, empty-string and empty-list patches", () => {
  const patch = fromJson(ConsoleBotPatchSchema, { id: "worker", revision: "18446744073709551615", enabled: false, description: "", tools: {} });
  const roundtrip = fromBinary(ConsoleBotPatchSchema, toBinary(ConsoleBotPatchSchema, patch));
  expect(toJson(ConsoleBotPatchSchema, roundtrip)).toEqual({ id: "worker", revision: "18446744073709551615", enabled: false, description: "", tools: {} });
  expect(roundtrip.instructions).toBeUndefined();
  expect(roundtrip.mayMessage).toBeUndefined();
 });
 it("preserves revisions beyond JS number precision", () => {
  const bot = fromJson(ConsoleBotSchema, { revision: "9007199254740993" });
  expect(bot.revision).toBe(9007199254740993n);
  expect(toJson(ConsoleBotSchema, bot).revision).toBe("9007199254740993");
 });
 it("uses generated enum JSON values", () => {
  const task = fromJson(TaskApprovalRecordSchema, { state: "TASK_APPROVAL_STATE_WAITING", revision: "12" });
  expect(toJson(TaskApprovalRecordSchema, task).state).toBe("TASK_APPROVAL_STATE_WAITING");
  expect(() => fromJson(TaskApprovalRecordSchema, { state: "WAITING" })).toThrow();
 });
});
