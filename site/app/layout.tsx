import type { Metadata } from "next";
import "./globals.css";

// 根 layout：唯一含 <html>/<body> 的布局（Next App Router 要求根 layout
// 必须有这两个标签）。locale 的 lang 属性无法在这里设置（根 layout 不
// 知道 locale）——默认 lang="en"，[locale]/layout 里用 NextIntlClientProvider
// 传 locale。静态导出下保持简单，不在此处读 segment 设 lang。
export const metadata: Metadata = {
  title: {
    default: "hwcloud — Pluggable Multi-Agent AI Framework in Go",
    template: "%s | hwcloud",
  },
  description:
    "A fully pluggable, multi-agent AI agent framework in Go. Every component is an interface — Model, Memory, Tools, Guards, Approver, Hooks, Observer — assembled by a 7-stage runtime with ACP v1, layered governance, and WASM plugins.",
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body className="min-h-screen flex flex-col">{children}</body>
    </html>
  );
}
