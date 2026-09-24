import type { Bucket } from "./api";

// When is how a bucket's start reads in Sydney: an hour or a day.
export function when(iso: string, bucket: "hour" | "day"): string {
  return new Intl.DateTimeFormat("en-AU", {
    timeZone: "Australia/Sydney",
    weekday: "short",
    day: "numeric",
    month: "short",
    ...(bucket === "hour" ? { hour: "numeric" } : {}),
  }).format(new Date(iso));
}

const W = 720;
const H = 200;
const LEFT = 40; // room for the percentage labels, so no bar covers them

// OnTimeChart draws on-time percentage per bucket as bars. Hand-drawn SVG: a
// bar per bucket and a 0–100 scale is not worth a charting library.
export function OnTimeChart({ buckets, bucket }: { buckets: Bucket[]; bucket: "hour" | "day" }) {
  const shown = buckets.filter((b) => b.on_time_pct !== null);
  if (shown.length === 0) {
    return (
      <p className="muted">
        No history in this range yet. Each hour is rolled up three hours after it ends, so history runs
        up to four hours behind.
      </p>
    );
  }
  const step = (W - LEFT) / shown.length;
  const gap = Math.min(4, step * 0.2);
  const summary = shown
    .map((b) => `${when(b.bucket_start, bucket)}: ${b.on_time_pct}% on time`)
    .join("; ");

  return (
    <svg viewBox={`0 -10 ${W} ${H + 30}`} className="chart" role="img" aria-label={summary}>
      {[0, 50, 100].map((pct) => (
        <g key={pct}>
          <line x1={LEFT} x2={W} y1={H - (pct / 100) * H} y2={H - (pct / 100) * H} className="grid" />
          <text x={LEFT - 6} y={H - (pct / 100) * H + 4} className="axis" textAnchor="end">
            {pct}%
          </text>
        </g>
      ))}
      {shown.map((b, i) => {
        const pct = b.on_time_pct ?? 0;
        const h = (pct / 100) * H;
        return (
          <rect
            key={b.bucket_start}
            x={LEFT + i * step + gap / 2}
            y={H - h}
            width={Math.max(1, step - gap)}
            height={h}
            className={pct >= 90 ? "bar good" : pct >= 75 ? "bar fair" : "bar poor"}
          >
            <title>
              {`${when(b.bucket_start, bucket)}: ${pct}% on time of ${b.n_obs} stop visits` +
                (b.delay_p50_s !== null ? `, median delay ${b.delay_p50_s} s` : "")}
            </title>
          </rect>
        );
      })}
      <text x={LEFT} y={H + 16} className="axis">
        {when(shown[0].bucket_start, bucket)}
      </text>
      <text x={W} y={H + 16} className="axis" textAnchor="end">
        {when(shown[shown.length - 1].bucket_start, bucket)}
      </text>
    </svg>
  );
}
