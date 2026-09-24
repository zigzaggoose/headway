"use client";

import { useEffect, useMemo, useState } from "react";
import { API, get, type History, type Line, type Now, type Status } from "./api";
import { OnTimeChart } from "./chart";

// route_type, as TfNSW's bundles use it, to the name a rider would pick from.
const MODES: Record<number, string> = { 2: "Trains", 401: "Metro", 1: "Metro", 4: "Ferries", 900: "Light rail", 0: "Light rail" };

const STATUS: Record<Status, string> = {
  early: "Early",
  on_time: "On time",
  late: "Late",
  very_late: "Very late",
  cancelled: "Cancelled",
  unknown: "Unknown",
};

function delay(s: number | null): string {
  if (s === null) return "—";
  if (s === 0) return "0 s";
  const sign = s < 0 ? "−" : "+";
  const a = Math.abs(s);
  return a < 60 ? `${sign}${a} s` : `${sign}${Math.floor(a / 60)} min ${a % 60} s`;
}

const RANGES = { hour: 48 * 3600e3, day: 14 * 86400e3 } as const;

export default function Page() {
  const [lines, setLines] = useState<Line[]>([]);
  const [routeID, setRouteID] = useState("");
  const [now, setNow] = useState<Now | null>(null);
  const [stop, setStop] = useState<{ id: string; name: string } | null>(null);
  const [bucket, setBucket] = useState<"hour" | "day">("day");
  const [history, setHistory] = useState<History | null>(null);
  const [error, setError] = useState("");

  // The selected line lives in the URL fragment, so a view can be shared.
  useEffect(() => {
    get<{ lines: Line[] }>("/v1/lines?limit=1000")
      .then((r) => {
        setLines(r.lines.filter((l) => l.short_name || l.long_name));
        setRouteID(decodeURIComponent(location.hash.slice(1)));
      })
      .catch((e: Error) => setError(e.message));
  }, []);

  // Live trips, refreshed as often as the feeds are polled.
  useEffect(() => {
    if (!routeID) return;
    location.hash = encodeURIComponent(routeID);
    const ctl = new AbortController();
    const load = () =>
      get<Now>(`/v1/lines/${encodeURIComponent(routeID)}/now`, ctl.signal)
        .then((n) => {
          setNow(n);
          setError("");
        })
        .catch((e: Error) => e.name !== "AbortError" && setError(e.message));
    load();
    const timer = setInterval(load, 15_000);
    return () => {
      ctl.abort();
      clearInterval(timer);
    };
  }, [routeID]);

  // History for the line, or for one of its stops once one is picked.
  useEffect(() => {
    if (!routeID) return;
    const from = new Date(Date.now() - RANGES[bucket]).toISOString();
    const q = `bucket=${bucket}&from=${encodeURIComponent(from)}`;
    const path = stop
      ? `/v1/stops/${encodeURIComponent(stop.id)}/history?route_id=${encodeURIComponent(routeID)}&${q}`
      : `/v1/lines/${encodeURIComponent(routeID)}/history?${q}`;
    const ctl = new AbortController();
    get<History>(path, ctl.signal)
      .then(setHistory)
      .catch((e: Error) => e.name !== "AbortError" && setError(e.message));
    return () => ctl.abort();
  }, [routeID, stop, bucket]);

  const byMode = useMemo(() => {
    const groups = new Map<string, Line[]>();
    for (const l of [...lines].sort((a, b) => (a.short_name || a.long_name).localeCompare(b.short_name || b.long_name, "en", { numeric: true }))) {
      const mode = MODES[l.route_type] ?? "Other";
      groups.set(mode, [...(groups.get(mode) ?? []), l]);
    }
    return groups;
  }, [lines]);

  const line = lines.find((l) => l.route_id === routeID);

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
        value={routeID}
        onChange={(e) => {
          setStop(null);
          setNow(null);
          setHistory(null);
          setRouteID(e.target.value);
        }}
      >
        <option value="">Choose a line…</option>
        {[...byMode].map(([mode, ls]) => (
          <optgroup key={mode} label={mode}>
            {ls.map((l) => (
              <option key={l.route_id} value={l.route_id}>
                {[l.short_name, l.long_name].filter(Boolean).join(" · ")}
              </option>
            ))}
          </optgroup>
        ))}
      </select>

      {error && (
        <p role="alert" className="error">
          {error}
        </p>
      )}

      {line && now && (
        <section aria-labelledby="now">
          <h2 id="now">
            {line.short_name || line.long_name} right now
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
            <table>
              <thead>
                <tr>
                  <th scope="col">To</th>
                  <th scope="col">Next stop</th>
                  <th scope="col">Delay</th>
                  <th scope="col">Status</th>
                </tr>
              </thead>
              <tbody>
                {now.trips.map((t) => (
                  <tr key={t.trip_id}>
                    <td>{t.headsign ?? "—"}</td>
                    <td>
                      <button
                        className="link"
                        aria-pressed={stop?.id === t.next_stop.stop_id}
                        onClick={() => setStop({ id: t.next_stop.stop_id, name: t.next_stop.name ?? t.next_stop.stop_id })}
                      >
                        {t.next_stop.name ?? t.next_stop.stop_id}
                      </button>
                    </td>
                    <td>{delay(t.next_stop.delay_s)}</td>
                    <td className={t.next_stop.status}>{STATUS[t.next_stop.status]}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </section>
      )}

      {line && (
        <section aria-labelledby="history">
          <h2 id="history">
            On time {stop ? `at ${stop.name}` : `on ${line.short_name || line.long_name}`}
            {history?.totals.on_time_pct != null && (
              <span className="muted">
                {" "}
                · {history.totals.on_time_pct}% of {history.totals.n_obs} stop visits
              </span>
            )}
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
          {history && <OnTimeChart buckets={history.buckets} bucket={bucket} />}
          <p className="muted small">Pick a next stop in the table to see that stop&apos;s history on this line.</p>
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
