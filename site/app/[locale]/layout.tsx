import { setRequestLocale } from "next-intl/server";
import { NextIntlClientProvider } from "next-intl";
import { getMessages } from "next-intl/server";
import { notFound } from "next/navigation";
import { routing, type Locale } from "@/i18n/routing";
import { Nav } from "@/components/nav";
import { Footer } from "@/components/footer";

// 静态导出：枚举所有 locale，build 时为每个 locale 生成一套静态页。
// setRequestLocale 让 next-intl 在静态渲染时知道当前 locale（不依赖
// middleware 注入 requestLocale）。
export function generateStaticParams() {
  return routing.locales.map((locale) => ({ locale }));
}

type Props = {
  params: Promise<{ locale: string }>;
  children: React.ReactNode;
};

// 注意：这里不包 <html>/<body>——根 app/layout.tsx 已包（Next 要求根
// layout 唯一持有这两个标签）。本 layout 只负责挂 IntlProvider + Nav +
// Footer，包裹 [locale] 下的所有页面。
export default async function LocaleLayout({ params, children }: Props) {
  const { locale } = await params;
  if (!routing.locales.includes(locale as Locale)) {
    notFound();
  }
  setRequestLocale(locale);

  const messages = await getMessages();

  return (
    <NextIntlClientProvider messages={messages} locale={locale}>
      <Nav locale={locale as Locale} />
      <main className="flex-1">{children}</main>
      <Footer />
    </NextIntlClientProvider>
  );
}
