import Link from "next/link";
import { setRequestLocale, getTranslations } from "next-intl/server";
import {
  Puzzle,
  Network,
  Cable,
  RefreshCw,
  ShieldCheck,
  Boxes,
  ArrowRight,
} from "lucide-react";
import { ArchitectureDiagram } from "@/components/architecture-diagram";
import { CodeBlock } from "@/components/code-block";
import { TerminalWindow } from "@/components/terminal-window";
import { Callout } from "@/components/callout";

const REPO = "https://github.com/yusheng-g/hwcloud";

const FEATURE_ICONS = {
  pluggable: Puzzle,
  multiagent: Network,
  acp: Cable,
  runtime: RefreshCw,
  governance: ShieldCheck,
  plugins: Boxes,
} as const;

export default async function LandingPage({
  params,
}: {
  params: Promise<{ locale: string }>;
}) {
  const { locale } = await params;
  setRequestLocale(locale);
  const t = await getTranslations("landing");
  const featureKeys = Object.keys(FEATURE_ICONS) as (keyof typeof FEATURE_ICONS)[];
  const stack = t.raw("heroStack") as string[];
  const termLines = t.raw("terminal.lines") as {
    type: "cmd" | "ok" | "out" | "comment";
    text: string;
  }[];

  return (
    <>
      {/* Hero — 左右分栏：左文案+chip，右终端窗口 */}
      <section className="border-b border-border">
        <div className="mx-auto grid max-w-7xl gap-12 px-6 py-20 lg:grid-cols-2 lg:items-center lg:py-28">
          {/* 左：定位 + CTA + 技术栈 chip */}
          <div>
            <h1 className="font-mono text-5xl font-bold tracking-tight text-text">
              hwcloud
            </h1>
            <p className="mt-3 font-mono text-lg font-medium text-accent">
              {t("tagline")}
            </p>
            <p className="mt-6 max-w-xl text-base leading-relaxed text-muted">
              {t("subtitle")}
            </p>
            <div className="mt-8 flex flex-wrap gap-3">
              <a
                href={REPO}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex items-center gap-1.5 rounded-lg border border-border px-5 py-2.5 text-sm font-medium text-text transition-colors hover:border-accent hover:text-accent"
              >
                {t("cta.github")}
              </a>
              <Link
                href={`/${locale}/docs/architecture`}
                className="inline-flex items-center gap-1.5 rounded-lg bg-accent px-5 py-2.5 text-sm font-medium text-bg transition-colors hover:bg-accent-hover"
              >
                {t("cta.start")}
                <ArrowRight size={15} />
              </Link>
            </div>
            {/* 技术栈 / 特性 chip */}
            <div className="mt-6 flex flex-wrap gap-2">
              {stack.map((chip) => (
                <span
                  key={chip}
                  className="rounded-full border border-border bg-surface px-3 py-0.5 font-mono text-xs text-muted"
                >
                  {chip}
                </span>
              ))}
            </div>
          </div>

          {/* 右：仿终端窗口 */}
          <TerminalWindow title={t("terminal.title")} lines={termLines} />
        </div>
      </section>

      {/* 心智模型 callout */}
      <section className="mx-auto max-w-7xl px-6 py-10">
        <Callout>{t("mentalModel")}</Callout>
      </section>

      {/* Features grid — 深色 surface 卡 + 绿图标 + hover 左边框亮绿 */}
      <section className="mx-auto max-w-7xl px-6 py-16">
        <h2 className="mb-10 text-2xl font-bold text-text">{t("featuresTitle")}</h2>
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {featureKeys.map((key) => {
            const Icon = FEATURE_ICONS[key];
            return (
              <div
                key={key}
                className="rounded-xl border border-border bg-surface p-6 transition-colors hover:border-accent"
              >
                <Icon className="mb-3 text-accent" size={22} />
                <h3 className="mb-1.5 font-semibold text-text">
                  {t(`features.${key}.title`)}
                </h3>
                <p className="text-sm leading-relaxed text-muted">
                  {t(`features.${key}.desc`)}
                </p>
              </div>
            );
          })}
        </div>
      </section>

      {/* Architecture teaser — 7-stage 图在深底上贴终端味 */}
      <section className="border-y border-border bg-surface">
        <div className="mx-auto max-w-7xl px-6 py-16">
          <h2 className="mb-3 text-2xl font-bold text-text">{t("archTeaserTitle")}</h2>
          <p className="mb-6 max-w-3xl text-sm leading-relaxed text-muted">
            {t("archTeaserDesc")}
          </p>
          <ArchitectureDiagram />
          <div className="mt-6">
            <Link
              href={`/${locale}/docs/architecture`}
              className="inline-flex items-center gap-1 text-sm font-medium text-accent transition-colors hover:text-accent-hover"
            >
              {t("archReadMore")}
            </Link>
          </div>
        </div>
      </section>

      {/* Quick start — 代码块带终端标题栏 */}
      <section className="mx-auto max-w-7xl px-6 py-16">
        <h2 className="mb-3 text-2xl font-bold text-text">{t("quickstartTitle")}</h2>
        <p className="mb-8 text-sm text-muted">{t("prereq")}</p>

        <h3 className="mb-2 font-semibold text-text">{t("buildTitle")}</h3>
        <CodeBlock label="bash">{`go build -o hwcloud ./cmd/cli/

# Show version
./hwcloud -v

# ACP mode (stdio — for VSCode/Zed ACP plugins)
./hwcloud serve --acp

# REST mode (HTTP + SSE)
./hwcloud serve --port 8080`}</CodeBlock>

        <h3 className="mb-2 mt-6 font-semibold text-text">{t("configTitle")}</h3>
        <CodeBlock label="json">{`{
  "provider": {
    "openai": {
      "api_key": "sk-...",
      "models": ["gpt-4o"]
    }
  }
}`}</CodeBlock>

        <h3 className="mb-3 mt-6 font-semibold text-text">{t("firstRunTitle")}</h3>
        <ol className="max-w-3xl list-inside list-decimal space-y-2 text-sm leading-relaxed text-text">
          {t.raw("firstRun").map((step: string, i: number) => (
            <li key={i}>{step}</li>
          ))}
        </ol>
      </section>
    </>
  );
}
