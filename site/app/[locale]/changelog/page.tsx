import { setRequestLocale, getTranslations } from "next-intl/server";
import { routing } from "@/i18n/routing";

const REPO = "https://github.com/yusheng-g/hwcloud";

// 每个 [locale] 下的 page 都必须接收 params 并调 setRequestLocale——
// 否则 getTranslations 回退去读 headers(requestLocale) 触发动态渲染，
// 与 output:'export' 的 static-only 约束冲突。
export default async function ChangelogPage({
  params,
}: {
  params: Promise<{ locale: string }>;
}) {
  const { locale } = await params;
  setRequestLocale(locale);
  const t = await getTranslations("changelog");
  return (
    <article className="mx-auto max-w-3xl px-6 py-20">
      <h1 className="mb-4 text-3xl font-bold text-text">{t("title")}</h1>
      <p className="mb-6 text-base text-muted">{t("body")}</p>
      <a
        href={`${REPO}/releases`}
        target="_blank"
        rel="noopener noreferrer"
        className="inline-flex items-center gap-1 text-sm font-medium text-accent transition-colors hover:text-accent-hover"
      >
        {t("link")}
      </a>
    </article>
  );
}
