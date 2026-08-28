import { setRequestLocale, getTranslations } from "next-intl/server";
import { CodeBlock } from "@/components/code-block";

type ConceptItem = { name: string; desc: string; chips: string[]; example: string };
type ConceptGroup = { group: string; items: ConceptItem[] };

export default async function ConceptsPage({
  params,
}: {
  params: Promise<{ locale: string }>;
}) {
  const { locale } = await params;
  setRequestLocale(locale);
  const t = await getTranslations("docs.concept");
  const groups = t.raw("groups") as ConceptGroup[];

  return (
    <article>
      <h1 className="mb-3 text-3xl font-bold text-text">{t("title")}</h1>
      <p className="mb-12 text-base leading-relaxed text-muted">{t("intro")}</p>

      {groups.map((g) => (
        <section key={g.group} className="mb-14">
          {/* 分组标题：编号 + 组名 */}
          <h2 className="mb-5 flex items-center gap-3 border-b border-border pb-2 text-lg font-semibold text-text">
            <span className="font-mono text-accent">#</span>
            {g.group}
          </h2>
          <div className="grid gap-4 md:grid-cols-2">
            {g.items.map((item) => (
              <div
                key={item.name}
                className="flex flex-col rounded-xl border border-border bg-surface p-5 transition-colors hover:border-accent"
              >
                {/* 概念名 */}
                <h3 className="font-mono font-semibold text-text">{item.name}</h3>
                {/* 一句话定义 */}
                <p className="mt-1.5 mb-3 text-sm leading-relaxed text-muted">{item.desc}</p>
                {/* chips */}
                <div className="mb-3 flex flex-wrap gap-1.5">
                  {item.chips.map((chip) => (
                    <span
                      key={chip}
                      className="rounded-full border border-border bg-surface-2 px-2 py-0.5 font-mono text-[10px] text-accent"
                    >
                      {chip}
                    </span>
                  ))}
                </div>
                {/* 终端示例 */}
                <div className="mt-auto">
                  <CodeBlock>{item.example}</CodeBlock>
                </div>
              </div>
            ))}
          </div>
        </section>
      ))}
    </article>
  );
}
