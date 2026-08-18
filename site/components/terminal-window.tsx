import { cn } from "@/lib/utils";

// 仿 macOS 终端窗口——标题栏（红黄绿三圆点 + 标题）+ 暗底 monospace 内容。
// 内容是静态 hwcloud 会话演示（纯文字，不跑真命令）。hero 的视觉锚点：
// 开发者一眼认出"这是个 CLI 工具"。prompt $ 用绿，输出浅灰，✓ 状态绿。
//
// 行类型：cmd（带绿色 $ 前缀）、ok（绿 ✓ 前缀）、out（纯输出灰）、comment（# 注释灰）。
type Line = { type: "cmd" | "ok" | "out" | "comment"; text: string };

export function TerminalWindow({
  title,
  lines,
  className,
}: {
  title: string;
  lines: Line[];
  className?: string;
}) {
  return (
    <div
      className={cn(
        "overflow-hidden rounded-xl border border-border bg-surface shadow-2xl shadow-black/40",
        className,
      )}
    >
      {/* 标题栏：三圆点 + 标题 */}
      <div className="flex items-center gap-2 border-b border-border bg-surface-2 px-4 py-2.5">
        <span className="h-3 w-3 rounded-full bg-danger" />
        <span className="h-3 w-3 rounded-full bg-warn" />
        <span className="h-3 w-3 rounded-full bg-ok" />
        <span className="ml-2 font-mono text-xs text-muted">{title}</span>
      </div>
      {/* 终端内容 */}
      <div className="overflow-x-auto p-4 font-mono text-[13px] leading-relaxed">
        {lines.map((line, i) => {
          switch (line.type) {
            case "cmd":
              return (
                <div key={i} className="whitespace-pre">
                  <span className="text-prompt">$ </span>
                  <span className="text-text">{line.text}</span>
                </div>
              );
            case "ok":
              return (
                <div key={i} className="whitespace-pre text-accent">
                  ✓ {line.text}
                </div>
              );
            case "comment":
              return (
                <div key={i} className="whitespace-pre text-muted">
                  {line.text}
                </div>
              );
            default: // out
              return (
                <div key={i} className="whitespace-pre text-muted">
                  {line.text}
                </div>
              );
          }
        })}
      </div>
    </div>
  );
}
