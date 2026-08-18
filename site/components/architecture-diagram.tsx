"use client";

import { useTranslations } from "next-intl";
import { ArrowDown, ArrowRight } from "lucide-react";

// hwcloud 的真实架构图（基于代码核实，非 README 照搬）：
//   Application (rest / acp / cmd/cli) → kernel.Runtime（7-stage mainline loop）
//   → Deps 注入的独立顶层包（context/execution/governance/session/provider/eventbus）
//   → 可插拔 interface（Model/Tools/MCP）→ 传输层（ACP/REST/CLI/IM）
//
// 准确性依据（核对 agent/agent.go、kernel/runtime.go、kernel/run.go、observer.go）：
//   - Agent 是纯配置（agent.Agent 只含字段，无执行方法）
//   - kernel.Runtime.New(*agent.Agent, Deps) 分离配置与执行
//   - 7 个 stage：guard.in → memory.fetch → prompt.build → model.call
//                 → guard.out → tool.execute → memory.append
//     （policy + execution 合并在 executeTools，无独立 stage——非 README 的 8-node）
//   - context/execution/governance/session/provider/eventbus 是独立顶层包经 Deps DI 注入
//
// namespace 默认 "landing.arch"；docs 页传 "docs.arch" 复用结构但内容不同。
// 这里只画 landing 那张总览图；docs/architecture 页有更细的分层详解。
export function ArchitectureDiagram({
  namespace = "landing.arch",
}: {
  namespace?: string;
}) {
  const t = useTranslations(namespace);

  return (
    <div className="rounded-xl border border-border bg-surface p-6 font-mono">
      <Layer title={t("app.title")} sub={t("app.sub")} note={t("app.note")} />

      <Flow label={t("new")} />

      <Layer title={t("runtime.title")} sub={t("runtime.sub")} note={t("runtime.note")}>
        {/* 7-stage mainline loop — 横向流程 */}
        <div className="mt-3 rounded-lg border border-accent/30 bg-bg p-3">
          <div className="mb-1.5 text-[10px] text-muted">{t("loopLabel")}</div>
          <div className="flex flex-wrap items-center gap-1 text-[11px]">
            {t.raw("stages").map((s: string, i: number) => (
              <span key={i} className="flex items-center gap-1">
                <span className="rounded border border-border bg-surface-2 px-1.5 py-0.5 text-accent">
                  {s}
                </span>
                {i < (t.raw("stages") as string[]).length - 1 && (
                  <ArrowRight size={11} className="text-muted" />
                )}
              </span>
            ))}
          </div>
        </div>
      </Layer>

      <Flow label={t("deps")} />

      {/* Deps 注入的独立包 — 2×3 grid */}
      <div className="mt-1 grid grid-cols-2 gap-2 sm:grid-cols-3">
        <InnerBlock label={t("pkg.context")} />
        <InnerBlock label={t("pkg.execution")} />
        <InnerBlock label={t("pkg.governance")} />
        <InnerBlock label={t("pkg.session")} />
        <InnerBlock label={t("pkg.provider")} />
        <InnerBlock label={t("pkg.eventbus")} />
      </div>

      <Flow label={t("pluggable")} />

      <Layer title={t("iface.title")} sub={t("iface.sub")} note={t("iface.note")} />

      <Flow label={t("transport")} />

      <Layer title={t("out.title")} sub={t("out.sub")} note={t("out.note")} last />

      <div className="mt-4 flex items-center gap-2 border-t border-border pt-4 text-[11px] text-muted">
        <ArrowRight size={13} className="text-accent" />
        {t("deliver")}
      </div>
    </div>
  );
}

function Layer({
  title,
  sub,
  note,
  last,
  children,
}: {
  title: string;
  sub: string;
  note: string;
  last?: boolean;
  children?: React.ReactNode;
}) {
  return (
    <div
      className={
        last
          ? "rounded-lg border border-border bg-bg p-4"
          : "rounded-lg border border-accent/30 bg-bg p-4"
      }
    >
      <div className="flex items-baseline gap-2">
        <span className="text-sm font-semibold text-accent">{title}</span>
        <span className="text-[11px] text-muted">{sub}</span>
      </div>
      <div className="mt-0.5 text-[11px] text-muted">{note}</div>
      {children}
    </div>
  );
}

function InnerBlock({ label }: { label: string }) {
  return (
    <div className="rounded border border-border bg-surface-2 px-2 py-1.5 text-center text-[11px] text-muted">
      {label}
    </div>
  );
}

function Flow({ label }: { label: string }) {
  return (
    <div className="flex items-center justify-center gap-2 py-2">
      <ArrowDown size={14} className="text-accent" />
      <span className="rounded-full border border-border bg-surface-2 px-2 py-0.5 text-[10px] text-muted">
        {label}
      </span>
    </div>
  );
}
