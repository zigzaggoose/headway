"use client";

import { useEffect, useState } from "react";
import { get, type Status, type Trip, type TripDetail } from "./api";

const STATUS: Record<Status, string> = {
  early: "Early",
  on_time: "On time",
  late: "Late",
  very_late: "Very late",
  cancelled: "Cancelled",
  skipped: "Skipped",
  unknown: "No data",
};

function delay(s: number | null): string {
  if (s === null) return "—";
  if (s === 0) return "0 s";
  const sign = s < 0 ? "−" : "+";
  const a = Math.abs(s);
  return a < 60 ? `${sign}${a} s` : `${sign}${Math.floor(a / 60)} min ${a % 60} s`;
}

// time is an instant as a Sydney wall clock: "7:32 pm".
function time(iso: string | null | undefined): string {
  if (!iso) return "—";
  return new Intl.DateTimeFormat("en-AU", { timeZone: "Australia/Sydney", hour: "numeric", minute: "2-digit" }).format(
    new Date(iso),
  );
}

// station drops TfNSW's platform suffix: "Hornsby Station Platform 3" is Hornsby.
export const station = (name: string | undefined) => (name ?? "").replace(/ Station\b.*$/, "");

// TripRow is one train: when and where it starts and ends, where it is now,
// and, opened, every stop it calls at. The stops load only when opened and
// refresh with the rest of the page.
export function TripRow({
  trip,
  refreshed,
  onStop,
}: {
  trip: Trip;
  refreshed: unknown;
  onStop: (s: { id: string; name: string }) => void;
}) {
  const [open, setOpen] = useState(false);
  const [detail, setDetail] = useState<TripDetail | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!open) return;
    const ctl = new AbortController();
    get<TripDetail>(`/v1/trips/${encodeURIComponent(trip.trip_id)}?service_date=${trip.service_date}`, ctl.signal)
      .then((d) => {
        setDetail(d);
        setError("");
      })
      .catch((e: Error) => e.name !== "AbortError" && setError(e.message));
    return () => ctl.abort();
  }, [open, trip.trip_id, trip.service_date, refreshed]);

  const n = trip.next_stop;
  return (
    <details className="trip" onToggle={(e) => setOpen(e.currentTarget.open)}>
      <summary>
        <span className="journey">
          {trip.start && trip.end ? (
            <>
              <b>{time(trip.start.scheduled)}</b> {station(trip.start.name)} → <b>{time(trip.end.scheduled)}</b>{" "}
              {station(trip.end.name)}
            </>
          ) : (
            <>To {trip.headsign ?? "an unknown stop"}</>
          )}
        </span>
        <span className="where">
          <span className={n.status}>{n.status === "cancelled" ? STATUS.cancelled : delay(n.delay_s)}</span>
          <span className="muted"> · next {station(n.name) || n.stop_id}</span>
        </span>
      </summary>
      {error && <p className="error">{error}</p>}
      {open && !detail && !error && <p className="muted">Loading stops…</p>}
      {detail && (
        <table>
          <thead>
            <tr>
              <th scope="col">Stop</th>
              <th scope="col">Timetable</th>
              <th scope="col">Delay</th>
              <th scope="col">Status</th>
            </tr>
          </thead>
          <tbody>
            {detail.stops.map((s) => (
              <tr key={s.stop_sequence} className={s.passed ? undefined : "upcoming"}>
                <td>
                  <button className="link" onClick={() => onStop({ id: s.stop_id, name: s.name ?? s.stop_id })}>
                    {s.name?.replace(" Station Platform ", " · Platform ") ?? s.stop_id}
                  </button>
                </td>
                <td>{time(s.scheduled)}</td>
                <td>{delay(s.delay_s)}</td>
                <td className={s.status}>
                  {STATUS[s.status]}
                  {!s.passed && s.status !== "unknown" && <span className="muted"> (expected)</span>}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </details>
  );
}
