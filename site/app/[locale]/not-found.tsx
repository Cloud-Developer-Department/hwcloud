import Link from "next/link";
import { useTranslations } from "next-intl";
import { routing } from "@/i18n/routing";

// [locale] 下的 404 页（不存在的 docs 路径等）。
// 用 useTranslations（client 侧，layout 已挂 NextIntlClientProvider）。
export default function NotFound() {
  const t = useTranslations("notFound");
  // 默认回 en 首页——locale 未知时 routing.defaultLocale 兜底。
  const home = `/${routing.defaultLocale}`;
  return (
    <div className="mx-auto max-w-3xl px-6 py-24 text-center">
      <h1 className="mb-3 text-3xl font-bold text-text">{t("title")}</h1>
      <p className="mb-6 text-muted">{t("body")}</p>
      <Link href={home} className="text-sm font-medium text-accent hover:text-accent-hover">
        {t("home")}
      </Link>
    </div>
  );
}
