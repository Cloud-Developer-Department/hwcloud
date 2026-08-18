// 代码块：终端窗口风——标题栏（红黄绿点 + 可选 label）+ surface 底 monospace。
// 和 hero 的 TerminalWindow 同款标题栏，让 quickstart 的代码块也像终端窗口。
// 纯静态渲染，不做语法高亮（避免引入 prism/shiki，保持依赖精简）。
export function CodeBlock({ children, label }: { children: string; label?: string }) {
  return (
    <div className="my-3 overflow-hidden rounded-xl border border-border bg-surface">
      <div className="flex items-center gap-2 border-b border-border bg-surface-2 px-4 py-2">
        <span className="h-2.5 w-2.5 rounded-full bg-danger" />
        <span className="h-2.5 w-2.5 rounded-full bg-warn" />
        <span className="h-2.5 w-2.5 rounded-full bg-ok" />
        {label && <span className="ml-2 font-mono text-xs text-muted">{label}</span>}
      </div>
      <pre className="overflow-auto p-4 text-[13px] leading-relaxed text-code-text font-mono">
        <code>{children}</code>
      </pre>
    </div>
  );
}
