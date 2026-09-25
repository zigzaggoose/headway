// node --test app/lines.test.ts — real names from the live /v1/lines.
import { test } from "node:test";
import assert from "node:assert/strict";
import type { Line, Status, Trip } from "./api.ts";
import { reverseOf, samePattern, shown, tally } from "./lines.ts";

const line = (route_id: string, short_name: string, long_name: string): Line => ({
  route_id,
  short_name,
  long_name,
  route_type: 2,
  feed_id: "sydneytrains",
});

const lines = [
  line("WST_1a", "T1", "City to Emu Plains"),
  line("WST_2c", "T1", "Emu Plains to Berowra via City "),
  line("NSN_2k", "T1", "Berowra to Emu Plains via City"),
  line("NSN_1a", "T1", "City to Hornsby and Berowra via Gordon"),
  line("NSN_2a", "T1", "Berowra and Hornsby to City via Gordon"),
  line("SHL_1a", "SHL", "Central to Campbelltown and Goulburn via Granville"),
  line("SHL_1b", "SHL", "Central to Campbelltown and Goulburn via Regents Park"),
  line("SHL_2a", "SHL", "Goulburn to Campbelltown and Central via Granville"),
  line("IWL_1a", "T2", "City Circle to Parramatta"),
  line("IWL_2b", "T2", "Parramatta to City Circle "),
  line("IWL_2a", "T2", "Parramatta to City Circle "),
  line("T6_2a", "T6", "Bankstown to Lidcombe"),
  line("T3_9z", "T3", "Lidcombe to Bankstown"),
  line("F1", "F1", "Manly"),
];
const rev = (id: string) => reverseOf(lines.find((l) => l.route_id === id)!, lines)?.route_id;

test("a pattern reverses to the one with its ends swapped", () => {
  assert.equal(rev("WST_2c"), "NSN_2k");
  assert.equal(rev("NSN_2k"), "WST_2c");
});

test("places joined by and match in either order", () => {
  assert.equal(rev("NSN_1a"), "NSN_2a");
  assert.equal(rev("SHL_2a"), "SHL_1a");
});

test("the via has to match", () => {
  assert.equal(rev("SHL_1b"), undefined);
});

test("of two identical reverses the first route_id wins", () => {
  assert.equal(rev("IWL_1a"), "IWL_2a");
});

test("another line with the same stations is not a reverse", () => {
  assert.equal(rev("T6_2a"), undefined);
});

test("no reverse published, or not a journey, is undefined", () => {
  assert.equal(rev("WST_1a"), undefined);
  assert.equal(rev("F1"), undefined);
});

const bucket = (early: number, on_time: number, late: number, very_late: number, cancelled: number) => ({
  bucket_start: "2026-09-24T00:00:00+10:00",
  n_obs: early + on_time + late + very_late + cancelled,
  n_early: early,
  n_on_time: on_time,
  n_late: late,
  n_very_late: very_late,
  n_cancelled: cancelled,
  on_time_pct: null,
  delay_p50_s: null,
});
const history = (...buckets: ReturnType<typeof bucket>[]) => ({
  totals: { n_obs: 0, on_time_pct: null, delay_p50_s: null },
  buckets,
});

test("tally adds every bucket of every route", () => {
  const t = tally([history(bucket(1, 10, 2, 1, 3), bucket(0, 5, 0, 0, 0)), history(bucket(1, 5, 2, 0, 1))]);
  assert.deepEqual(t, { early: 2, on_time: 20, late: 4, very_late: 1, cancelled: 4 });
});

test("routes with one name are one pattern, whatever TfNSW's padding", () => {
  assert.deepEqual(
    samePattern(lines.find((l) => l.route_id === "IWL_2b")!, lines).map((l) => l.route_id),
    ["IWL_2a", "IWL_2b"],
  );
});

const trip = (trip_id: string, from: string, to: string, next: string, status: Status): Trip => ({
  trip_id,
  service_date: "2026-09-25",
  start: { stop_id: "a", name: `${from} Station Platform 1`, scheduled: null },
  end: { stop_id: "b", name: `${to} Station Platform 2`, scheduled: null },
  matched: true,
  next_stop: { stop_id: "c", name: `${next} Station Platform 3`, delay_s: 0, status },
});
const trips = [
  trip("1", "Hornsby", "Central", "Chatswood", "on_time"),
  trip("2", "Central", "Hornsby", "Wynyard", "very_late"),
  trip("3", "Berowra", "Central", "Gordon", "late"),
  trip("4", "Hornsby", "Berowra", "Hornsby", "cancelled"),
];
const ids = (ts: Trip[]) => ts.map((t) => t.trip_id);

test("shown narrows by status, late taking very late too", () => {
  assert.deepEqual(ids(shown(trips, "", "all")), ["1", "2", "3", "4"]);
  assert.deepEqual(ids(shown(trips, "", "late")), ["2", "3"]);
  assert.deepEqual(ids(shown(trips, "", "cancelled")), ["4"]);
});

test("shown needs every searched word in some station, any case", () => {
  assert.deepEqual(ids(shown(trips, "hornsby CENTRAL", "all")), ["1", "2"]);
  assert.deepEqual(ids(shown(trips, "  gordon ", "all")), ["3"]);
  assert.deepEqual(ids(shown(trips, "hornsby", "late")), ["2"]);
  assert.deepEqual(ids(shown(trips, "parramatta", "all")), []);
});
