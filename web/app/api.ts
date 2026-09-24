// The §7 response shapes this page reads, and one fetch helper.

// NEXT_PUBLIC_ variables are inlined at build time. Local development against
// a local API: NEXT_PUBLIC_API_URL=http://localhost:8080 npm run dev, with
// HTTP_CORS_ORIGIN=http://localhost:3000 in the API's .env.
export const API = process.env.NEXT_PUBLIC_API_URL ?? "https://api.transitlateagain.dev";

export type Line = {
  route_id: string;
  short_name: string;
  long_name: string;
  route_type: number;
  feed_id: string;
};

export type Status = "early" | "on_time" | "late" | "very_late" | "cancelled" | "unknown";

export type Trip = {
  trip_id: string;
  headsign?: string;
  matched: boolean;
  next_stop: {
    stop_id: string;
    name?: string;
    scheduled?: string;
    predicted?: string;
    delay_s: number | null;
    status: Status;
  };
};

export type Now = {
  as_of: string | null;
  feed_age_s: number | null;
  summary: {
    active_trips: number;
    early: number;
    on_time: number;
    late: number;
    very_late: number;
    cancelled: number;
    median_delay_s: number | null;
  };
  trips: Trip[];
};

export type Bucket = {
  bucket_start: string;
  n_obs: number;
  n_early: number;
  n_on_time: number;
  n_late: number;
  n_very_late: number;
  n_cancelled: number;
  on_time_pct: number | null;
  delay_p50_s: number | null;
};

export type History = {
  name?: string;
  totals: { n_obs: number; on_time_pct: number | null; delay_p50_s: number | null };
  buckets: Bucket[];
};

// get returns the parsed body, or throws the API's own message: every non-2xx
// carries the §7.2 envelope, and a 429 says how long to wait.
export async function get<T>(path: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(API + path, { signal });
  const body = await res.json().catch(() => null);
  if (!res.ok) {
    if (res.status === 429) {
      throw new Error(`Too many requests; try again in ${res.headers.get("Retry-After") ?? "a few"} s.`);
    }
    throw new Error(body?.error?.message ?? `The API answered ${res.status}.`);
  }
  return body as T;
}
