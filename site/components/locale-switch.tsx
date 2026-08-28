"use client";

import { useParams, usePathname, useRouter } from "next/navigation";
import { useTranslations } from "next-intl";
import { routing, type Locale } from "@/i18n/routing";
import { cn } from "@/lib/utils";

// 客户端语言切换器。静态导出下不能靠 middleware 做 locale 路由——
// 切语言 = 把当前 pathname 里的 /en 或 /zh 段替换掉，然后 router.push。
// 保留路径其余部分（docs/architecture 切到 zh 后仍是 docs/architecture）。
export function LocaleSwitch() {
  const router = useRouter();
  const pathname = usePathname();
  const params = useParams<{ locale: string }>();
  const t = useTranslations("localeSwitch");
  const current = params.locale as Locale;

  function switchTo(next: Locale) {
    if (next === current) return;
    // usePathname() 在含 basePath 的配置下返回不含 basePath 的路径
    // （/en/docs/architecture，不是 /hwcloud/en/...）。所以直接替换
    // 开头的 locale 段即可，router.push 会自动补回 basePath。
    const stripped = pathname.replace(/^\//, "").split("/").filter(Boolean);
    if (stripped.length > 0 && routing.locales.includes(stripped[0] as Locale)) {
      stripped[0] = next;
    } else {
      stripped.unshift(next);
    }
    router.push("/" + stripped.join("/") + "/");
  }

  return (
    <div className="inline-flex items-center rounded-md border border-border text-xs" aria-label={t("label")}>
      {routing.locales.map((loc, i) => (
        <button
          key={loc}
          onClick={() => switchTo(loc)}
          className={cn(
            "px-2 py-1 transition-colors",
            loc === current
              ? "bg-accent text-white"
              : "text-muted hover:text-text hover:bg-surface-2",
            i > 0 && "border-l border-border",
          )}
        >
          {t(loc)}
        </button>
      ))}
    </div>
  );
}
