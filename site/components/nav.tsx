import Link from "next/link";
import { useTranslations } from "next-intl";
import { LocaleSwitch } from "./locale-switch";
import type { Locale } from "@/i18n/routing";

const REPO = "https://github.com/yusheng-g/hwcloud";

// 顶栏：wordmark + 版本 badge + 导航 + GitHub + 语言切换。
// 深色底（bg-bg/80 + backdrop-blur）；wordmark 用 monospace 突出 developer 味。
export function Nav({ locale }: { locale: Locale }) {
  const t = useTranslations("nav");
  return (
    <header className="sticky top-0 z-50 border-b border-border bg-bg/80 backdrop-blur">
      <div className="mx-auto flex h-14 max-w-7xl items-center gap-6 px-6">
        <Link
          href={`/${locale}`}
          className="flex items-center gap-2 font-mono font-semibold tracking-tight text-text"
        >
          hwcloud
          <span className="rounded-full border border-border bg-surface px-2 py-0.5 text-[10px] font-normal text-muted">
            v0.0.1-beta.1
          </span>
        </Link>
        <nav className="flex items-center gap-5 text-sm text-muted">
          <Link href={`/${locale}/docs/architecture`} className="transition-colors hover:text-accent">
            {t("docs")}
          </Link>
          <Link href={`/${locale}/changelog`} className="transition-colors hover:text-accent">
            {t("changelog")}
          </Link>
        </nav>
        <div className="ml-auto flex items-center gap-4">
          <LocaleSwitch />
          <a
            href={REPO}
            target="_blank"
            rel="noopener noreferrer"
            className="text-sm text-muted transition-colors hover:text-accent"
          >
            {t("github")}
          </a>
        </div>
      </div>
    </header>
  );
}
