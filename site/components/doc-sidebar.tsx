"use client";

import Link from "next/link";
import { usePathname, useParams } from "next/navigation";
import { useTranslations } from "next-intl";
import { cn } from "@/lib/utils";
import type { Locale } from "@/i18n/routing";

// docs 侧边栏——深色 surface 底，sticky 全高。
// 层级靠缩进区分（组标题左 0，项左 pl-5），不用前缀符号——纯文字 + 缩进是
// docs 侧边栏的通行做法（GitHub/Stripe/Vercel docs 都这样），加 - 或 > 反而冗余。
// active 项：实心绿左边框（border-l-2）+ 绿字 + 绿透明底；非 active hover 时
// 左边框预亮浅绿 + 底色变亮，给"将选中"的视觉提示。
export function DocSidebar() {
  const pathname = usePathname();
  const params = useParams<{ locale: string }>();
  const locale = params.locale as Locale;
  const t = useTranslations("docs");

  const groups: { label: string; items: { href: string; key: string }[] }[] = [
    {
      label: t("groupOverview"),
      items: [
        { href: `/${locale}/docs/architecture`, key: "architecture" },
        { href: `/${locale}/docs/concepts`, key: "concepts" },
        { href: `/${locale}/docs/iac-server`, key: "iac-server" },
      ],
    },
    {
      label: t("groupReference"),
      items: [
        { href: `/${locale}/docs/plugins`, key: "plugins" },
        { href: `/${locale}/changelog`, key: "changelog" },
      ],
    },
  ];

  return (
    <nav className="sticky top-0 h-screen w-[260px] shrink-0 overflow-auto border-r border-border bg-surface p-4">
      {/* wordmark + 版本 */}
      <div className="mb-7 px-2">
        <div className="font-mono text-base font-semibold text-text">hwcloud</div>
        <div className="text-xs text-muted">{t("sidebarVersion")}</div>
      </div>

      {groups.map((g) => (
        <div key={g.label} className="mb-6">
          {/* 组标题：小号、大写、muted，紧贴左边 */}
          <div className="mb-2 px-2 text-[11px] font-semibold uppercase tracking-wider text-muted/60">
            {g.label}
          </div>
          {/* 组项：缩进 pl-5（比组标题的 px-2 往右一档），层级清晰 */}
          {g.items.map((it) => {
            const active = pathname.replace(/\/$/, "") === it.href.replace(/\/$/, "");
            return (
              <Link
                key={it.key}
                href={it.href}
                className={cn(
                  "mb-0.5 block border-l-2 py-1.5 pl-5 pr-3 text-[13px] transition-colors",
                  active
                    ? "border-accent bg-accent-soft text-accent"
                    : "border-transparent text-muted hover:border-border hover:bg-surface-2 hover:text-text",
                )}
              >
                {t(it.key)}
              </Link>
            );
          })}
        </div>
      ))}
    </nav>
  );
}
