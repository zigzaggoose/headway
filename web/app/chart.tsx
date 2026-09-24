import type { Tally } from "./lines";

// Late folds in very late: five warm-to-red slices could not be told apart
// under colour blindness, and the legend still gives the very-late count.
// This order keeps every neighbouring pair, and the wrap from cancelled back
// to on time, distinguishable (validated in light and dark).
const SLICES = [
  { cls: "on-time", label: "On time", n: (t: Tally) => t.on_time },
  { cls: "early", label: "Early", n: (t: Tally) => t.early },
  { cls: "late", label: "Late", n: (t: Tally) => t.late + t.very_late },
  { cls: "cancelled", label: "Cancelled", n: (t: Tally) => t.cancelled },
];

const R = 80;

// arc is one slice from fraction a to b of the circle, clockwise from 12 o'clock.
function arc(a: number, b: number): string {
  const at = (f: number) => `${R * Math.sin(2 * Math.PI * f)} ${-R * Math.cos(2 * Math.PI * f)}`;
  return `M 0 0 L ${at(a)} A ${R} ${R} 0 ${b - a > 0.5 ? 1 : 0} 1 ${at(b)} Z`;
}

// StatusPie shows where the stop visits in the range ended up. Hand-drawn SVG:
// four arcs are not worth a charting library.
export function StatusPie({ tally }: { tally: Tally }) {
  const total = SLICES.reduce((s, x) => s + x.n(tally), 0);
  if (total === 0) {
    return (
      <p className="muted">
        No history in this range yet. Each hour is rolled up three hours after it ends, so history runs
        up to four hours behind.
      </p>
    );
  }
  const pct = (n: number) => `${Math.round((1000 * n) / total) / 10}%`;
  let from = 0;
  return (
    <div className="pie">
      <svg viewBox={`${-R - 2} ${-R - 2} ${2 * R + 4} ${2 * R + 4}`} aria-hidden="true">
        {SLICES.map((s) => {
          const n = s.n(tally);
          if (n === 0) return null;
          const a = from;
          from += n / total;
          return n === total ? (
            <circle key={s.cls} r={R} className={s.cls} />
          ) : (
            <path key={s.cls} d={arc(a, from)} className={s.cls}>
              <title>{`${s.label}: ${n} (${pct(n)})`}</title>
            </path>
          );
        })}
      </svg>
      <ul>
        {SLICES.map((s) => (
          <li key={s.cls}>
            <span className={`swatch ${s.cls}`} />
            {s.label} <b>{pct(s.n(tally))}</b>{" "}
            <span className="muted">
              {s.n(tally)}
              {s.cls === "late" && tally.very_late > 0 && `, ${tally.very_late} over 15 min`}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}
