import { getRequestConfig } from "next-intl/server";
import { routing } from "./routing";

// next-intl 静态导出模式：每个请求/导出页调用此函数，按 locale 加载
// 对应 messages。locale 来自 [locale] 路由参数（见 app/[locale]/layout.tsx
// 的 setRequestLocale 调用），不依赖 middleware 注入。
export default getRequestConfig(async ({ requestLocale }) => {
  let locale = await requestLocale;
  if (!locale || !routing.locales.includes(locale as never)) {
    locale = routing.defaultLocale;
  }
  return {
    locale,
    messages: (await import(`../messages/${locale}.json`)).default,
  };
});
