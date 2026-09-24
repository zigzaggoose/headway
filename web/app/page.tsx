"use client";

import { useEffect, useMemo, useState } from "react";
import { API, get, type History, type Line, type Now } from "./api";
import { StatusPie } from "./chart";
import { mergeNow, name, reverseOf, samePattern, tally as sum, type Tally } from "./lines";
import { TripRow } from "./trip";

// route_type, as TfNSW's bundles use it, to the name a rider would pick from.
const MODES: Record<number, string> = { 2: "Trains", 401: "Metro", 1: "Metro", 4: "Ferries", 900: "Light rail", 0: "Light rail" };

const RANGES = { hour: 48 * 3600e3, day: 14 * 86400e3 } as const;

export default function Page() {
  const [lines, setLines] = useState<Line[]>([]);
  const [routeID, setRouteID] = useState("");
  const [now, setNow] = useState<Now | null>(null);
  const [stop, setStop] = useState<{ id: string; name: string } | null>(null);
  const [bucket, setBucket] = useState<"hour" | "day">("day");
  const [tally, setTally] = useState<Tally | null>(null);
  const [error, setError] = useState("");

  const line = lines.find((l) => l.route_id === routeID);
  // Every route shown as the selected one; its first is the one in the menu.
  const same = line ? samePattern(line, lines) : [];
  const ids = same.map((l) => l.route_id).join(",");
  // One entry per name: the first route of each samePattern group.
  const patterns = line
    ? lines
        .filter((l) => l.short_name === line.short_name)
        .filter((l, _, ls) => samePattern(l, ls)[0] === l)
        .sort((a, b) => a.route_id.localeCompare(b.route_id))
    : [];
  const reverse = line && reverseOf(line, lines);
  const title = line && (patterns.length > 1 ? `${line.short_name} ${name(line)}` : line.short_name);

  const pick = (id: string) => {
    setStop(null);
    setNow(null);
    setTally(null);
    setRouteID(id);
  };

  // The selected line lives in the URL fragment, so a view can be shared.
  useEffect(() => {
    get<{ lines: Line[] }>("/v1/lines?limit=1000")
      .then((r) => {
        // No short name is TfNSW's RTTA_* empty-train routes, never a rider's.
        setLines(r.lines.filter((l) => l.short_name));
        setRouteID(decodeURIComponent(location.hash.slice(1)));
      })
      .catch((e: Error) => setError(e.message));
  }, []);

  // Live trips, refreshed as often as the feeds are polled.
  useEffect(() => {
    if (!ids) return;
    location.hash = encodeURIComponent(routeID);
    const ctl = new AbortController();
    const load = () =>
      Promise.all(ids.split(",").map((id) => get<Now>(`/v1/lines/${encodeURIComponent(id)}/now`, ctl.signal)))
        .then((ns) => {
          setNow(mergeNow(ns));
          setError("");
        })
        .catch((e: Error) => e.name !== "AbortError" && setError(e.message));
    load();
    const timer = setInterval(load, 15_000);
    return () => {
      ctl.abort();
      clearInterval(timer);
    };
  }, [routeID, ids]);

  // History for the route, or for one of its stops once one is picked.
  useEffect(() => {
    if (!ids) return;
    const from = new Date(Date.now() - RANGES[bucket]).toISOString();
    const q = `bucket=${bucket}&from=${encodeURIComponent(from)}`;
    const path = (id: string) =>
      stop
        ? `/v1/stops/${encodeURIComponent(stop.id)}/history?route_id=${encodeURIComponent(id)}&${q}`
        : `/v1/lines/${encodeURIComponent(id)}/history?${q}`;
    const ctl = new AbortController();
    Promise.all(ids.split(",").map((id) => get<History>(path(id), ctl.signal)))
      .then((hs) => setTally(sum(hs)))
      .catch((e: Error) => e.name !== "AbortError" && setError(e.message));
    return () => ctl.abort();
  }, [ids, stop, bucket]);

  // Mode → short name → that line's routes, the main pattern (_1a) first.
  const byMode = useMemo(() => {
    const groups = new Map<string, Map<string, Line[]>>();
    const sorted = [...lines].sort(
      (a, b) => a.short_name.localeCompare(b.short_name, "en", { numeric: true }) || a.route_id.localeCompare(b.route_id),
    );
    for (const l of sorted) {
      const mode = MODES[l.route_type] ?? "Other";
      const names = groups.get(mode) ?? groups.set(mode, new Map()).get(mode)!;
      names.set(l.short_name, [...(names.get(l.short_name) ?? []), l]);
    }
    return groups;
  }, [lines]);


  return (
    <main>
      <header>
        <h1>Transit Late Again</h1>
        <p className="muted">
          Live running and on-time history for Sydney&apos;s trains, metro, ferries and light rail, measured from
          Transport for NSW&apos;s realtime feeds.
        </p>
      </header>

      <label htmlFor="line">Line</label>
      <select
        id="line"
        value={line?.short_name ?? ""}
        onChange={(e) => pick(lines.find((l) => l.short_name === e.target.value)?.route_id ?? "")}
      >
        <option value="">Choose a line…</option>
        {[...byMode].map(([mode, names]) => (
          <optgroup key={mode} label={mode}>
            {[...names].map(([name, ls]) => (
              <option key={name} value={name}>
                {ls.length === 1 ? `${name} · ${ls[0].long_name}` : name}
              </option>
            ))}
          </optgroup>
        ))}
      </select>

      {patterns.length > 1 && (
        <>
          <label htmlFor="route">Route</label>
          <select id="route" value={same[0]?.route_id} onChange={(e) => pick(e.target.value)}>
            {patterns.map((l) => (
              <option key={l.route_id} value={l.route_id}>
                {name(l)}
              </option>
            ))}
          </select>
          {reverse && (
            <div className="controls reverse">
              <button onClick={() => pick(reverse.route_id)}>⇄ Reverse: {name(reverse)}</button>
            </div>
          )}
        </>
      )}

      {error && (
        <p role="alert" className="error">
          {error}
        </p>
      )}

      {line && now && (
        <section aria-labelledby="now">
          <h2 id="now">
            {title} right now
            {now.feed_age_s !== null && <span className="muted"> · data {now.feed_age_s} s old</span>}
          </h2>
          <ul className="summary">
            <li>
              <b>{now.summary.active_trips}</b> running
            </li>
            <li className="on_time">
              <b>{now.summary.on_time}</b> on time
            </li>
            <li className="late">
              <b>{now.summary.late + now.summary.very_late}</b> late
            </li>
            <li className="early">
              <b>{now.summary.early}</b> early
            </li>
            <li className="cancelled">
              <b>{now.summary.cancelled}</b> cancelled
            </li>
          </ul>
          {now.trips.length === 0 ? (
            <p className="muted">Nothing running on this line right now.</p>
          ) : (
            <div className="trips">
              {/* Earliest start first; a train the timetable lacks goes last. */}
              {[...now.trips]
                .sort((a, b) => (a.start?.scheduled ?? "~").localeCompare(b.start?.scheduled ?? "~"))
                .map((t) => (
                  <TripRow key={`${t.service_date} ${t.trip_id}`} trip={t} refreshed={now} onStop={setStop} />
                ))}
            </div>
          )}
        </section>
      )}

      {line && (
        <section aria-labelledby="history">
          <h2 id="history">
            On time {stop ? `at ${stop.name}` : `on ${title}`}
          </h2>
          <div className="controls">
            <button aria-pressed={bucket === "day"} onClick={() => setBucket("day")}>
              Last 14 days
            </button>
            <button aria-pressed={bucket === "hour"} onClick={() => setBucket("hour")}>
              Last 48 hours
            </button>
            {stop && <button onClick={() => setStop(null)}>Whole line</button>}
          </div>
          {tally && <StatusPie tally={tally} />}
          <p className="muted small">Open a train and pick one of its stops to see that stop&apos;s history on this route.</p>
        </section>
      )}

      <footer className="muted small">
        On time is up to 1 min early or 5 min late. Data: Transport for NSW Open Data. API:{" "}
        <a href={`${API}/v1/lines`}>{API.replace(/^https?:\/\//, "")}</a> ·{" "}
        <a href="https://github.com/zigzaggoose/transitlateagain">source</a>
      </footer>
    </main>
  );
}
