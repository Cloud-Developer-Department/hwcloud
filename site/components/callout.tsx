import { cn } from "@/lib/utils";

// 强调/注意块：info = 绿左边框 + 绿透明底（accent-soft）；warn = 橙左边框。
// 深色版：用 accent-soft（绿 rgba 12%）作 info 底，warn 用 warn 色 + 透明底。
export function Callout({
  type = "info",
  title,
  children,
}: {
  type?: "info" | "warn";
  title?: string;
  children: React.ReactNode;
}) {
  return (
    <div
      className={cn(
        "my-4 rounded-r-lg p-4",
        type === "info"
          ? "bg-accent-soft border-l-4 border-accent"
          : "border-l-4 border-warn",
      )}
    >
      {title && <p className="mb-1 font-semibold text-text">{title}</p>}
      <div className="text-sm text-text">{children}</div>
    </div>
  );
}
