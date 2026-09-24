import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "Transit Late Again",
  description:
    "Live running and on-time history for Sydney's trains, metro, ferries and light rail, from Transport for NSW's realtime feeds.",
};

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en-AU">
      <body>{children}</body>
    </html>
  );
}
