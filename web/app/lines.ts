import type { History, Line, Now } from "./api";

// TfNSW has no route that is "T1": it publishes one route_id per stopping
// pattern and direction ("City to Emu Plains", "Emu Plains to Berowra via City"
// ...), all with short name T1. The page groups them back under the short name.

// places splits "Bondi Junction and Central" into its stations, sorted, so the
// two directions of a pattern compare equal however each lists them.
function places(s: string): string[] {
  return s.split(" and ").map((p) => p.trim()).sort();
}

// ends reads "A to B via C" as from = A, and a key shared by both directions:
// every place at either end, plus the via. null for names that are not a
// journey, such as the ferries' "Manly".
function ends(l: Line): { from: string; key: string } | null {
  const [journey, via = ""] = l.long_name.split(" via ");
  const [a, b] = journey.split(" to ");
  if (b === undefined) return null;
  return { from: places(a).join("|"), key: [...places(a), ...places(b)].sort().join("|") + "/" + places(via).join("|") };
}

// reverseOf is the same-line route running the other way, or undefined when
// TfNSW publishes none: "City to Emu Plains" has no "Emu Plains to City".
// Of several, the first by route_id, which is TfNSW's main pattern (_1a, _2a).
export function reverseOf(l: Line, lines: Line[]): Line | undefined {
  const e = ends(l);
  if (!e) return undefined;
  return lines
    .filter((o) => o.short_name === l.short_name && o.route_id !== l.route_id)
    .sort((a, b) => a.route_id.localeCompare(b.route_id))
    .find((o) => {
      const f = ends(o);
      return f !== null && f.key === e.key && f.from !== e.from;
    });
}

// name is a route's long name as a rider reads it; TfNSW pads some with a space.
export const name = (l: Line) => l.long_name.trim();

// samePattern is every route with l's line and name. TfNSW gives some stopping
// patterns identical names (IWL_1a and IWL_1b are both "City Circle to
// Parramatta"); the page shows them as one and adds their data together.
export function samePattern(l: Line, lines: Line[]): Line[] {
  return lines
    .filter((o) => o.short_name === l.short_name && name(o) === name(l))
    .sort((a, b) => a.route_id.localeCompare(b.route_id));
}

// mergeNow adds several routes' live states. The median delay does not add,
// and the page does not show it, so a merged one is null.
export function mergeNow(ns: Now[]): Now {
  if (ns.length === 1) return ns[0];
  const sum = (k: "active_trips" | "early" | "on_time" | "late" | "very_late" | "cancelled") =>
    ns.reduce((s, n) => s + n.summary[k], 0);
  return {
    ...ns[0],
    summary: {
      active_trips: sum("active_trips"),
      early: sum("early"),
      on_time: sum("on_time"),
      late: sum("late"),
      very_late: sum("very_late"),
      cancelled: sum("cancelled"),
      median_delay_s: null,
    },
    trips: ns.flatMap((n) => n.trips),
  };
}

export type Tally = { early: number; on_time: number; late: number; very_late: number; cancelled: number };

// tally sums every bucket of every history: counts add exactly across hours,
// days and routes, unlike the percentiles.
export function tally(hs: History[]): Tally {
  const t: Tally = { early: 0, on_time: 0, late: 0, very_late: 0, cancelled: 0 };
  for (const b of hs.flatMap((h) => h.buckets)) {
    t.early += b.n_early;
    t.on_time += b.n_on_time;
    t.late += b.n_late;
    t.very_late += b.n_very_late;
    t.cancelled += b.n_cancelled;
  }
  return t;
}
