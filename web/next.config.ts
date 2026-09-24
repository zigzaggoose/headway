import type { NextConfig } from "next";

// A static export: Cloudflare Pages serves the files and the browser calls
// the API directly (PROJECT.md §15).
const nextConfig: NextConfig = {
  output: "export",
};

export default nextConfig;
