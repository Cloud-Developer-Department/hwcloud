import { setRequestLocale, getTranslations } from "next-intl/server";
import { ArchitectureDiagram } from "@/components/architecture-diagram";
import { CodeBlock } from "@/components/code-block";

type Layer = { title: string; desc: string; chips: string[]; example: string };
type StackRow = { layer: string; stack: string };

export default async function ArchitecturePage({
  params,
}: {
  params: Promise<{ locale: string }>;
}) {
  const { locale } = await params;
  setRequestLocale(locale);
  const t = await getTranslations("docs.arch");
  const layers = t.raw("layers") as Layer[];
  const stack = t.raw("stack") as StackRow[];

  return (
    <article>
      <h1 className="mb-3 text-3xl font-bold text-text">{t("title")}</h1>
      <p className="mb-8 text-base leading-relaxed text-muted">{t("intro")}</p>

      {/* 视觉架构图 */}
      <ArchitectureDiagram namespace="landing.arch" />

      {/* 分层详解：每层一卡片，配 chips + 数据流示例 */}
      <section className="mt-12">
        <h2 className="mb-5 border-b border-border pb-2 text-lg font-semibold text-text">
          <span className="font-mono text-accent">#</span> {t("layersTitle")}
        </h2>
        <div className="space-y-4">
          {layers.map((l) => (
            <div
              key={l.title}
              className="rounded-xl border border-border bg-surface p-5 transition-colors hover:border-accent"
            >
              <div className="flex flex-wrap items-baseline gap-3">
                <h3 className="font-mono font-semibold text-accent">{l.title}</h3>
                <div className="flex flex-wrap gap-1.5">
                  {l.chips.map((chip) => (
                    <span
                      key={chip}
                      className="rounded-full border border-border bg-surface-2 px-2 py-0.5 font-mono text-[10px] text-muted"
                    >
                      {chip}
                    </span>
                  ))}
                </div>
              </div>
              <p className="mt-2 mb-3 text-sm leading-relaxed text-muted">{l.desc}</p>
              <CodeBlock>{l.example}</CodeBlock>
            </div>
          ))}
        </div>
      </section>

      {/* 技术栈 */}
      <section className="mt-12">
        <h2 className="mb-5 border-b border-border pb-2 text-lg font-semibold text-text">
          <span className="font-mono text-accent">#</span> {t("stackTitle")}
        </h2>
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {stack.map((row) => (
            <div
              key={row.layer}
              className="rounded-xl border border-border bg-surface p-4"
            >
              <div className="mb-1 font-mono text-xs font-semibold text-accent">{row.layer}</div>
              <div className="text-sm text-muted">{row.stack}</div>
            </div>
          ))}
        </div>
      </section>
    </article>
  );
}
